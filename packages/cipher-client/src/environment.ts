// packages/cipher-client/src/environment.ts
//
// The variables every program holding one of these identities reads, so a deployment writes one
// set of values and the Go client (`packages/envoy/internal/cipherclient/environment.go`) and this
// one are told the same things the same way.

import { readFile } from "node:fs/promises";
import { join } from "node:path";
import { PROJECT_KEY_PATTERN, PROJECT_KEY_RULE } from "@legion/contracts/dispatch-tools";
import { isHttpUrl } from "@legion/contracts/log-text";
import { DeviceLoginIdentity, deviceLoginSessionName } from "./device-login";
import { DEVICE_LOGIN_FILE_VAR, deviceLoginFile } from "./device-login-file";
import { type DpopKey, importDpopKey, newDpopKey } from "./dpop";
import { readErrorCode } from "./error-text";
import {
  ACTIONS_ID_TOKEN_REQUEST_TOKEN_VAR,
  ACTIONS_ID_TOKEN_REQUEST_URL_VAR,
  githubActionsIdentity,
} from "./github-actions-identity";
import { MACHINE_AUDIENCE_VAR } from "./machine-audience";
import { metadataIdentity } from "./metadata-identity";
import { personIdentity } from "./person-identity";
import { poolIdentity } from "./pool-identity";
import { projectedToken } from "./projected-token";
import {
  chooseSignIn,
  PERSON_TRADE_VAR,
  PERSON_VAR,
  personTradeOff,
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
import { SCOPE_READ } from "./wire";

/** Names the secrets broker, as it does for every other form of `agent-secrets`. */
export const BROKER_URL_VAR = "AGENT_SECRETS_URL";
/** Says which identity this machine proves. Unset, this machine has none and nothing trades,
 *  which is every deployment that has not turned the exchange on. */
export const IDENTITY_VAR = "AGENT_SECRETS_MACHINE_IDENTITY";
/** The projected Kubernetes service-account token file, which {@link IDENTITY_KUBERNETES} reads
 *  and {@link IDENTITY_POOL} federates. */
export const MACHINE_TOKEN_FILE_VAR = "AGENT_SECRETS_MACHINE_TOKEN_FILE";
/** The audience of this machine's identity token, held in `machine-audience.ts`, which imports
 *  nothing, so the identities this module builds read it too. */
export { MACHINE_AUDIENCE_VAR };
/** The directory a session's agent-secrets identity lives in: in a pod, the role's own, where
 *  `agent-secrets keygen` wrote {@link ROLE_KEY_FILE} before the role's first hello (Go's
 *  `KeyDirVar`). {@link brokerTokenClient} reads it beside {@link IDENTITY_VAR}. */
export const KEY_DIR_VAR = "AGENT_SECRETS_KEY_DIR";
/** The key in {@link KEY_DIR_VAR}'s directory: the PKCS#8 P-256 PEM `agent-secrets keygen`
 *  writes (Go's `RoleKeyFile`). */
export const ROLE_KEY_FILE = "key.pem";
/** The session a trade writes under for a caller that acts as no session of its own, which a
 *  daemon, a command and a helper are ({@link tradeSession}). */
export const SESSION_VAR = "AGENT_SECRETS_SESSION";
/** The projects a session works on, project keys separated by white space, which a person's
 *  trade asks the broker to name in its token ({@link tradeProjects}; Go's `ProjectsVar`). The
 *  broker names only projects the person may see. */
export const PROJECTS_VAR = "AGENT_SECRETS_PROJECTS";
/** The most projects one token names (`oauthwire.MaxProjects`). */
export const MAX_PROJECTS = 16;
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
/** A GitHub Actions run's OIDC token, asked of GitHub at {@link ACTIONS_ID_TOKEN_REQUEST_URL_VAR}
 *  with {@link ACTIONS_ID_TOKEN_REQUEST_TOKEN_VAR} for {@link MACHINE_AUDIENCE_VAR}
 *  (`github-actions-identity.ts`): a job that holds `id-token: write`. */
export const IDENTITY_GITHUB_ACTIONS = "github-actions";
/** A machine's device login at the broker's device grant, kept in the file
 *  `AGENT_SECRETS_DEVICE_LOGIN_FILE` names (`device-login.ts`), which `dispatch login --device`
 *  makes: a machine with no platform identity of its own, such as a person's laptop. Only this
 *  client trades one; the Go client's `MachineIdentity` knows no such kind. */
export const IDENTITY_DEVICE_LOGIN = "device-login";

/**
 * The variables this package reads, by name: this module's, `github-actions-identity.ts`'s
 * `ACTIONS_ID_TOKEN_REQUEST_URL` and `ACTIONS_ID_TOKEN_REQUEST_TOKEN`, `device-login.ts`'s
 * `AGENT_SECRETS_DEVICE_LOGIN_FILE`, and those `signins.ts` reads to find a person's sign-in on
 * this machine and the client that renews it, whose names that module
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
  KEY_DIR_VAR,
  MACHINE_AUDIENCE_VAR,
  SESSION_VAR,
  PROJECTS_VAR,
  POOL_AUDIENCE_VAR,
  POOL_ACCOUNT_VAR,
  ACTIONS_ID_TOKEN_REQUEST_URL_VAR,
  ACTIONS_ID_TOKEN_REQUEST_TOKEN_VAR,
  DEVICE_LOGIN_FILE_VAR,
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
 * (`packages/envoy/internal/cipherclient/environment.go`), in the same order; a device login,
 * which Go has no kind for, reads the broker it was made at and the file that keeps it. Go's also
 * takes `github-actions`, a CI job's own identity, which this client never trades: a job's
 * `dispatch result` holds the proxy placeholder of its run, and the broker holds the run's
 * token.
 */
export function machineIdentity(env: BrokerEnvironment): TokenSource | undefined {
  const kind = value(env, IDENTITY_VAR);
  switch (kind) {
    case "":
      return undefined;
    case IDENTITY_KUBERNETES:
      return projectedToken(required(env, MACHINE_TOKEN_FILE_VAR, kind), MACHINE_TOKEN_FILE_VAR);
    case IDENTITY_METADATA:
      return metadataIdentity(required(env, MACHINE_AUDIENCE_VAR, kind));
    case IDENTITY_POOL: {
      const subject = required(env, MACHINE_TOKEN_FILE_VAR, kind);
      const tokenAudience = required(env, MACHINE_AUDIENCE_VAR, kind);
      const audience = required(env, POOL_AUDIENCE_VAR, kind);
      const serviceAccount = required(env, POOL_ACCOUNT_VAR, kind);
      return poolIdentity({
        subject: projectedToken(subject, MACHINE_TOKEN_FILE_VAR),
        audience,
        serviceAccount,
        tokenAudience,
      });
    }
    case IDENTITY_GITHUB_ACTIONS: {
      const requestUrl = required(env, ACTIONS_ID_TOKEN_REQUEST_URL_VAR, kind);
      const requestToken = required(env, ACTIONS_ID_TOKEN_REQUEST_TOKEN_VAR, kind);
      const audience = required(env, MACHINE_AUDIENCE_VAR, kind);
      return githubActionsIdentity({ requestUrl, requestToken, audience });
    }
    case IDENTITY_DEVICE_LOGIN: {
      const brokerUrl = required(env, BROKER_URL_VAR, kind);
      return new DeviceLoginIdentity({ path: deviceLoginFile(env), brokerUrl });
    }
    default:
      // Its value is not quoted: a secret pasted into the wrong variable would be printed.
      throw new Error(
        `${IDENTITY_VAR} names no identity this machine can prove; it takes ${IDENTITY_KUBERNETES}, ${IDENTITY_METADATA}, ${IDENTITY_POOL}, ${IDENTITY_GITHUB_ACTIONS} or ${IDENTITY_DEVICE_LOGIN}`
      );
  }
}

/**
 * The session a trade writes under, which the broker records the minted token against: `own`,
 * the session the caller is acting as, where it acts as one, and otherwise `AGENT_SECRETS_SESSION`.
 * The Go client reads the same rule out of the same two places
 * (`packages/envoy/internal/cipherclient/environment.go`, `TradeSession`).
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

/**
 * The projects a person's trade asks for: {@link PROJECTS_VAR}, project keys separated by white
 * space, answered sorted and each once, and empty when it is blank. A value that is no list — a
 * key that is not a project key, a key named twice, more than {@link MAX_PROJECTS} — throws
 * naming the variable, so a session never trades for fewer projects than it was started with.
 * The Go client reads the same variable by the same grammar
 * (`packages/envoy/internal/cipherclient/environment.go`, `TradeProjects`, through
 * `oauthwire.ParseProjects`).
 */
export function tradeProjects(env: BrokerEnvironment): string[] {
  const keys = value(env, PROJECTS_VAR)
    .split(/\s+/)
    .filter((key) => key !== "");
  if (keys.length > MAX_PROJECTS) {
    throw new Error(
      `${PROJECTS_VAR}: ${keys.length} projects are named, and a token names at most ${MAX_PROJECTS}`
    );
  }
  for (const key of keys) {
    if (!PROJECT_KEY_PATTERN.test(key)) {
      throw new Error(`${PROJECTS_VAR}: "${key}" is not a project key: ${PROJECT_KEY_RULE}`);
    }
  }
  const sorted = keys.sort();
  for (let at = 1; at < sorted.length; at += 1) {
    if (sorted[at] === sorted[at - 1]) {
      throw new Error(
        `${PROJECTS_VAR}: ${sorted[at]} is named twice, and a list names each project once`
      );
    }
  }
  return sorted;
}

/** What a refusal of the role's key calls it: the key in the directory {@link KEY_DIR_VAR} names,
 *  by the variable and never the path, which can be a token a person pasted where the directory
 *  goes (Go's `MachineKey` names it the same way). */
const ROLE_KEY_NAMED = `this role's agent-secrets key in the directory ${KEY_DIR_VAR} names`;

/**
 * The key a machine trade proves (Go's `MachineKey`). A role in a pod, where {@link IDENTITY_VAR}
 * is {@link IDENTITY_KUBERNETES} and {@link KEY_DIR_VAR} names the role's own agent-secrets
 * directory, proves the key in that directory's {@link ROLE_KEY_FILE}, because the broker refuses
 * a pod session's exchange whose DPoP key is not the one its registration names, which is that
 * key. A file there that cannot be read, or is no P-256 key, throws naming the directory by
 * {@link KEY_DIR_VAR} and a read's failure by its code ({@link readErrorCode}), with no cause,
 * since the runtime's error repeats the path: a fresh key in its place would be refused at the
 * broker in words that name neither the file nor the key. Every other machine proves a key of
 * this process's own, made here.
 */
async function machineKey(env: BrokerEnvironment): Promise<DpopKey> {
  const dir = value(env, KEY_DIR_VAR);
  if (value(env, IDENTITY_VAR) !== IDENTITY_KUBERNETES || dir === "") return await newDpopKey();
  let pem: string;
  try {
    pem = await readFile(join(dir, ROLE_KEY_FILE), "utf8");
  } catch (error) {
    throw new Error(`read ${ROLE_KEY_NAMED}: ${readErrorCode(error)}`);
  }
  return await importDpopKey(pem, ROLE_KEY_NAMED);
}

/** A client {@link brokerTokenClient} made, and the sign-in it trades. */
export interface MadeBrokerClient {
  readonly client: BrokerTokenClient;
  /**
   * The sign-in the client trades, as its person and its box key's thumbprint, where it trades a
   * person's; undefined where it trades this machine's own identity. The pair names a sign-in as
   * the broker records one (the person and the thumbprint of the key the proof is signed with),
   * so a caller that remembers a sign-in refused for good keys the refusal by it, as Go's
   * `SignInAuthority.refused` does (`packages/envoy/internal/cipherclient/signinauthority.go`):
   * only a new `agent-secrets login`, which makes a box key of its own, cures that refusal.
   */
  readonly signIn: Pick<SignInRecord, "person" | "thumbprint"> | undefined;
}

/** A trade narrowed to reading, as a caller of {@link brokerTokenClient} asks for one: the scope
 *  `read` ({@link SCOPE_READ}), and the cut where the token reads as of a past time. */
export interface ReadTrade {
  /** The cut, an RFC 3339 time sent as written (`FORM_AS_OF`); absent or empty for none. */
  readonly asOf?: string | undefined;
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
 * (`personDeclinesNothing`, `packages/envoy/internal/cipher/oauth/exchange.go`). It names the
 * projects {@link PROJECTS_VAR} lists ({@link tradeProjects}), which the broker names in the token
 * only when Dispatch says that person may see each one; the machine's own client below names
 * none, since its entry decides its project and the broker refuses one it is told. Every error
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
 * With no sign-in, it is the machine's own client. Its key is {@link machineKey}'s: a pod role's
 * own agent-secrets key, or one this process makes here, once per client; either way only its
 * public half ever leaves the process. The session is {@link tradeSession}'s. It declines the
 * launcher credential its identity holds (`launcherCredential`): every caller of this factory
 * wants a Dispatch access token and nothing else, and none of them reads `credentialId`. A key is
 * this process's and this session's, so asking would write a `launcher_credentials` row and an
 * audit row for every `dispatch` call, every Claude Code hook run, every `envoy-send` and every
 * session of a long-lived Oh My Pi or Claude Code process, each of them on the operator's
 * machine-logins list for its hour and removed by nothing. The machine's own launcher credential
 * is `agent-secrets`' and the helper's, which ask for it.
 *
 * A device login ({@link IDENTITY_DEVICE_LOGIN}) is none of those: it is a person's own
 * credential on their own machine, approved by them in Dispatch, so no sign-in is chosen beside
 * it and there is no actor. The login's access token is the subject (`device-login.ts`), the key
 * is the login's own, which the broker binds a trade of that token to, and the session is
 * {@link tradeSession}'s, else {@link deviceLoginSessionName}'s. It names the projects
 * {@link PROJECTS_VAR} lists, which the broker names only when Dispatch says the login's person
 * may see each one, and sends no launcher credential parameter: a trade of a machine login's
 * token mints no launcher credential. A machine with no login yet, or whose login file cannot be
 * read, is an error naming `dispatch login --device`. A process that turns the person trade off
 * ({@link PERSON_TRADE_VAR} set to `off`) is refused one: the broker mints its trade as the
 * login's person, so a controller would act on Dispatch and Envoy in its operator's name, and
 * there is no machine identity to trade in its place.
 *
 * Where `read` is given, each of the three clients asks for the scope read ({@link SCOPE_READ})
 * and the cut `read` names: the token keeps the identity the same trade would have had, and only
 * reads. The person's client and the device login's prove a key that signs launcher proofs, the
 * box key or the login's key, and the broker binds no read token to such a key, so each makes a
 * key of its own for the client and sends it as the holder key (`holderKey`): the token is bound
 * to that key, which signs every proof presented beside it, and the proof key signs the trade
 * alone. Neither sends `launcher_credential`, which the broker reads as declined for a read
 * trade. The machine's own client proves a key that holds no launcher credential, one this
 * process made or a pod role's agent-secrets key, which the broker enrolls as a session key and
 * mints no launcher credential on, and needs no holder key.
 */
export async function brokerTokenClient(
  env: BrokerEnvironment,
  own = "",
  read?: ReadTrade | undefined
): Promise<MadeBrokerClient | undefined> {
  const brokerUrl = value(env, BROKER_URL_VAR);
  const machine = machineIdentity(env);
  if (brokerUrl === "" || machine === undefined) return undefined;
  // Not quoted: every trade's text names this URL, and a value that is none may be a token pasted
  // into the variable.
  if (!isHttpUrl(brokerUrl)) throw new Error(`${BROKER_URL_VAR} is not an http or https URL`);
  const narrowed = read === undefined ? {} : { scope: SCOPE_READ, asOf: read.asOf };
  // A read trade of a client whose key signs launcher proofs, the person's or the device login's,
  // with a holder key made for that client alone.
  const heldApart = async () =>
    read === undefined ? {} : { ...narrowed, holderKey: await newDpopKey() };
  if (machine instanceof DeviceLoginIdentity) {
    if (personTradeOff(env)) {
      throw new Error(
        `${PERSON_TRADE_VAR}=off turns off every trade made as a person, and ${IDENTITY_VAR}=${IDENTITY_DEVICE_LOGIN} trades this machine's device login as the person who approved it; unset one of them`
      );
    }
    const client = new BrokerTokenClient({
      brokerUrl,
      key: await machine.key(),
      subject: machine,
      session: tradeSession(env, own) || deviceLoginSessionName(await machine.thumbprint()),
      projects: tradeProjects(env),
      ...(await heldApart()),
    });
    return { client, signIn: undefined };
  }
  const record = await chooseSignIn(env);
  if (record === undefined) {
    const client = new BrokerTokenClient({
      brokerUrl,
      key: await machineKey(env),
      subject: machine,
      session: tradeSession(env, own),
      launcherCredential: false,
      ...narrowed,
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
    projects: tradeProjects(env),
    ...(await heldApart()),
  });
  return { client, signIn: { person: record.person, thumbprint: record.thumbprint } };
}
