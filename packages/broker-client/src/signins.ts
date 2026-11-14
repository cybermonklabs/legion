// packages/broker-client/src/signins.ts
//
// Where a person's sign-in on this machine lives, and which person a session acts for, read as
// the Go client reads them (`packages/envoy/internal/brokerclient/signinstore.go`,
// `signinauthority.go` and `environment.go`).
//
// `agent-secrets login` makes a box key, runs the issuer's device grant for the person at the
// keyboard, and keeps the two together in one record per person: the key the broker binds the
// sign-in to, and the refresh token that mints the person's next identity token. This module
// reads those records and never writes one; the Go client is the only writer. It also reads the
// sign-in client a deployment configures, which renews the person's identity token from the
// refresh token a record holds.
//
// The variable names live here rather than in `environment.ts` because this module is the one
// that reads them, as `metadata-identity.ts` holds `GCE_METADATA_HOST`, and because the factory
// in `environment.ts` builds on this module: a constant it held and this module imported would
// be an import cycle.

import type { Dirent } from "node:fs";
import { readdir, readFile } from "node:fs/promises";
import { join } from "node:path";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import { messageOf } from "./error-text";

/** Names the directory a machine keeps its sign-ins in, for a deployment that places it itself
 *  (Go's `SignInDirVar`). Unset, {@link signInDirectory} answers the XDG state directory. */
export const SIGNIN_DIR_VAR = "AGENT_SECRETS_SIGNIN_DIR";
/** Names which person a session acts for where the machine holds more than one sign-in (Go's
 *  `PersonVar`). */
export const PERSON_VAR = "AGENT_SECRETS_PERSON";
/**
 * A setting of this process's own broker credentials that turns the person trade off: `off` is
 * its one value, under which {@link chooseSignIn} answers no sign-in and the client trades this
 * machine as where nobody signed in; unset or blank, the person trade is on. A process sets it
 * where its own credentials are a service's and never a person's: `legion controller start` sets
 * it (`packages/daemon/cmd/legion/controller.go`, `controllerPersonTradeVar`), so the
 * controller's own Dispatch and Envoy credentials trade the machine and never the sign-in its
 * operator made with {@link SIGNIN_COMMAND}, which the controller finds under the operator's
 * HOME.
 *
 * It is read by this TypeScript client alone: Go's `agent-secrets` and `brokerclient` never read
 * it, so the secrets a process reads through `agent-secrets` are signed as they would be without
 * it. Its name lies outside `AGENT_SECRETS_*` because `agent-secrets NAME -- <cmd>` strips every
 * variable of that prefix but a few from its child (`packages/envoy/cmd/agent-secrets/main.go`,
 * `sessionIdentityVars` and `buildChildEnv`), and a `dispatch` command run that way would lose
 * the setting and trade the person.
 */
export const PERSON_TRADE_VAR = "PERSON_SIGNIN_TRADE";
/** The OpenID provider a person signs in at, Google's unless a dev stack says otherwise (Go's
 *  `SignInIssuerVar`). */
export const SIGNIN_ISSUER_VAR = "AGENT_SECRETS_SIGNIN_ISSUER";
/** The device-flow client a person signed in with (Go's `SignInClientIDVar`). */
export const SIGNIN_CLIENT_ID_VAR = "AGENT_SECRETS_SIGNIN_CLIENT_ID";
/** The file holding that client's secret (Go's `SignInClientSecretFileVar`). The secret is in a
 *  file, never in a variable: a variable is read by every child of the shell that set it. */
export const SIGNIN_CLIENT_SECRET_FILE_VAR = "AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE";
/** Where every real person signs in (Go's `DefaultSignInIssuer`). */
export const DEFAULT_SIGNIN_ISSUER = "https://accounts.google.com";
/** The command every refusal that a sign-in would cure names (Go's `SignInCommand`). */
export const SIGNIN_COMMAND = "agent-secrets login";

/** The directory under the state directory that holds one record per person (Go's
 *  `SignInsDir`). */
const SIGNINS_DIR = "signins";

/** An environment these functions read their variables out of: a process's own, or a caller's
 *  record of one. `environment.ts`'s `BrokerVariables` names every variable they read; the two
 *  named here are the ones read by property, the base of the default sign-in directory. */
type Environment = Readonly<Record<string, string | undefined>> & {
  readonly XDG_STATE_HOME?: string | undefined;
  readonly HOME?: string | undefined;
};

/**
 * One person's sign-in on this machine as `agent-secrets login` keeps it on disk (Go's
 * `SignInRecord`), under the names the file spells its members in. A member the file leaves out
 * reads as "", as Go reads it.
 */
export interface SignInRecord {
  /** The address the sign-in proves (the identity token's `email` claim), and the name a session
   *  is told to act as. */
  readonly person: string;
  /** The provider the sign-in was made at. A renewal goes to the issuer the deployment names
   *  ({@link SignInFlow}), as Go's does. */
  readonly issuer: string;
  /** The device-flow client the sign-in was made with, which must be the client the deployment
   *  names now, since no other client can renew it. */
  readonly client_id: string;
  /** Mints the person's next identity token with no step by them. */
  readonly refresh_token: string;
  /** The box key `agent-secrets login` made, PKCS#8 PEM: the key the broker bound the sign-in to,
   *  which every trade of it proves. */
  readonly box_key: string;
  /** That key's RFC 7638 JWK thumbprint, which the broker records for the sign-in. */
  readonly thumbprint: string;
  /** The enrollment this person's session on this machine registered under, "" until one is
   *  made. */
  readonly enrollment_id: string;
  /** When the device grant completed, as the file writes it (RFC 3339). */
  readonly signed_in_at: string;
}

/** The members of {@link SignInRecord}, each read out of a record's JSON as a string. */
const RECORD_MEMBERS = [
  "person",
  "issuer",
  "client_id",
  "refresh_token",
  "box_key",
  "thumbprint",
  "enrollment_id",
  "signed_in_at",
] as const satisfies readonly (keyof SignInRecord)[];

/**
 * The device-flow client a person signed in with, as the deployment configures it (Go's
 * `DeviceFlow`, which `SignInFlow` builds): the issuer a person's identity token is renewed at,
 * and the client and secret that renewal authenticates as.
 */
export interface SignInFlow {
  /** The OpenID provider, with no trailing slash. */
  readonly issuer: string;
  readonly clientId: string;
  /** The client's secret, read from its file and trimmed. Google registers a limited-input
   *  device client as confidential, so the secret is sent; it is no secret of the person's. */
  readonly clientSecret: string;
}

/**
 * The directory this process's sign-ins live in, as Go's `SignIns` reads it:
 * {@link SIGNIN_DIR_VAR} where it names one, else `$XDG_STATE_HOME/agent-secrets/signins`, else
 * `$HOME/.local/state/agent-secrets/signins`.
 *
 * `XDG_STATE_HOME` is read before `HOME`, and that order is what keeps a Legion tmux pane off
 * the operator's sign-ins: the tmux runtime moves `XDG_STATE_HOME` under the daemon's own state
 * directory (`packages/daemon/internal/runtime/tmux/environment.go`, `xdgDirectories`). The
 * values are taken as they are set, untrimmed, so both clients name one directory for one
 * environment.
 */
export function signInDirectory(env: Environment): string {
  const named = env[SIGNIN_DIR_VAR] ?? "";
  if (named !== "") return named;
  const state = env.XDG_STATE_HOME ?? "";
  const base = state !== "" ? state : join(env.HOME ?? "", ".local", "state");
  return join(base, "agent-secrets", SIGNINS_DIR);
}

/**
 * One record, as Go's `SignInStore.Read` reads it: a file whose JSON holds no person, no refresh
 * token or no box key is refused naming the file and {@link SIGNIN_COMMAND}. Nothing of the
 * file's text reaches a refusal, since the file holds a refresh token and a private key: a
 * parser's own message can quote the text it stopped at.
 */
async function readSignIn(path: string): Promise<SignInRecord> {
  let text: string;
  try {
    text = await readFile(path, "utf8");
  } catch (error) {
    throw new Error(`read the sign-in at ${path}: ${messageOf(error)}`, { cause: error });
  }
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    throw new Error(`read the sign-in at ${path}: it is not JSON; run: ${SIGNIN_COMMAND}`);
  }
  if (typeof parsed !== "object" || parsed === null || Array.isArray(parsed)) {
    throw new Error(`read the sign-in at ${path}: it holds no JSON object; run: ${SIGNIN_COMMAND}`);
  }
  const fields = parsed as Record<string, unknown>;
  const record: Record<string, string> = {};
  for (const member of RECORD_MEMBERS) {
    // A member left out, or written as null, reads as "", as Go's `json.Unmarshal` leaves it.
    const given = fields[member] ?? "";
    if (typeof given !== "string") {
      throw new Error(
        `read the sign-in at ${path}: its ${member} is not a string; run: ${SIGNIN_COMMAND}`
      );
    }
    record[member] = given;
  }
  const read = record as unknown as SignInRecord;
  if (read.person === "" || read.refresh_token === "" || read.box_key === "") {
    throw new Error(
      `the sign-in at ${path} names no person, no refresh token or no box key; run: ${SIGNIN_COMMAND}`
    );
  }
  return read;
}

/**
 * Every sign-in in `dir`, by person, as Go's `SignInStore.List` reads them: each `*.json` entry
 * that is not a directory, read in the order of its name. A directory that is not there is no
 * sign-ins and no error, since most machines have none; a record that cannot be read is an
 * error, never a sign-in passed over.
 */
export async function listSignIns(dir: string): Promise<SignInRecord[]> {
  let entries: Dirent[];
  try {
    entries = await readdir(dir, { withFileTypes: true });
  } catch (error) {
    if ((error as NodeJS.ErrnoException).code === "ENOENT") return [];
    throw new Error(`read this machine's sign-ins: ${messageOf(error)}`, { cause: error });
  }
  const names = entries
    .filter((entry) => !entry.isDirectory() && entry.name.endsWith(".json"))
    .map((entry) => entry.name)
    .sort();
  const records: SignInRecord[] = [];
  for (const name of names) records.push(await readSignIn(join(dir, name)));
  // Go's `strings.Compare`: byte order, never a locale's.
  return records.sort((a, b) => (a.person < b.person ? -1 : a.person > b.person ? 1 : 0));
}

/** The people signed in here, for a refusal to name them (Go's `namesOf`). */
function namesOf(records: readonly SignInRecord[]): string {
  return records.map((record) => record.person).join(", ");
}

/**
 * Whether {@link PERSON_TRADE_VAR} turns the person trade off: true for `off`, false where it is
 * unset or blank, and a refusal for any other value, which is a deployment that meant one of the
 * two and wrote neither. The value is trimmed and its case kept, as `DISPATCH_TOKEN_BROKER`'s is
 * (`packages/envoy-client/src/dispatch-config.ts`, `brokerRouteForced`).
 */
function personTradeOff(env: Environment): boolean {
  const written = (env[PERSON_TRADE_VAR] ?? "").trim();
  if (written === "") return false;
  if (written === "off") return true;
  throw new Error(
    `${PERSON_TRADE_VAR} is "${written}" and takes off or nothing: set it to off where this process's own credentials trade this machine and never a person signed in on it, and leave it unset where they trade the person signed in here`
  );
}

/**
 * The sign-in a session of this environment acts under, by the rules of Go's
 * `SignInStore.Choose`, with {@link PERSON_VAR} as the person named:
 *
 * - {@link PERSON_TRADE_VAR} is `off`: undefined, a machine that trades as itself, with the
 *   store not read at all, so a store that cannot be read or holds two records and names nobody
 *   refuses nothing; any other value it holds is an error naming it;
 * - nobody signed in here: undefined, which is a machine that trades as itself;
 * - a person named: that person's record, whatever the case of the address, or an error naming
 *   who is signed in and {@link SIGNIN_COMMAND};
 * - one record and nobody named: that record;
 * - two or more and nobody named: an error naming {@link PERSON_VAR} and the people, so a session
 *   never acts as one colleague under another's name.
 */
export async function chooseSignIn(env: Environment): Promise<SignInRecord | undefined> {
  if (personTradeOff(env)) return undefined;
  const records = await listSignIns(signInDirectory(env));
  const person = (env[PERSON_VAR] ?? "").trim().toLowerCase();
  if (records.length === 0) return undefined;
  if (person !== "") {
    const named = records.find((record) => record.person.toLowerCase() === person);
    if (named !== undefined) return named;
    throw new Error(
      `${PERSON_VAR} names ${person}, who is not signed in on this machine; signed in here: ${namesOf(records)}; run: ${SIGNIN_COMMAND}`
    );
  }
  if (records.length === 1) return records[0];
  throw new Error(
    `${records.length} people are signed in on this machine (${namesOf(records)}), so this session must say which one it acts for: set ${PERSON_VAR} to one of them`
  );
}

/**
 * The session name Go's `SessionFor` gives a sign-in: `signin-<person>`, then `-` and the first
 * 12 characters of the box key's thumbprint where the record has one. It is the person's own
 * name and the same on every trade of that sign-in, and it tells two boxes of one person apart.
 * A session name belongs to one identity at the broker, so a person's trade never asks under the
 * name this machine trades its own identity under.
 */
export function signInSessionName(record: Pick<SignInRecord, "person" | "thumbprint">): string {
  const name = `signin-${record.person}`;
  return record.thumbprint === "" ? name : `${name}-${record.thumbprint.slice(0, 12)}`;
}

/**
 * The sign-in client this environment configures, or undefined where it configures none (Go's
 * `SignInFlow`). {@link SIGNIN_CLIENT_ID_VAR} and {@link SIGNIN_CLIENT_SECRET_FILE_VAR} go
 * together, and one without the other is an error naming both. The secret is read from its file
 * here, so a refusal names the variable rather than failing at the first renewal, and no refusal
 * carries the secret. The issuer is {@link SIGNIN_ISSUER_VAR}, else
 * {@link DEFAULT_SIGNIN_ISSUER}, without its trailing slashes.
 */
export async function signInFlow(env: Environment): Promise<SignInFlow | undefined> {
  const clientId = (env[SIGNIN_CLIENT_ID_VAR] ?? "").trim();
  const secretFile = (env[SIGNIN_CLIENT_SECRET_FILE_VAR] ?? "").trim();
  if (clientId === "" && secretFile === "") return undefined;
  if (clientId === "" || secretFile === "") {
    throw new Error(
      `a person's sign-in needs both ${SIGNIN_CLIENT_ID_VAR} and ${SIGNIN_CLIENT_SECRET_FILE_VAR}; this machine has one of them`
    );
  }
  let secret: string;
  try {
    secret = await readFile(secretFile, "utf8");
  } catch (error) {
    throw new Error(
      `read the sign-in client's secret (${SIGNIN_CLIENT_SECRET_FILE_VAR}): ${messageOf(error)}`,
      { cause: error }
    );
  }
  const issuer = (env[SIGNIN_ISSUER_VAR] ?? "").trim();
  return {
    issuer: withoutTrailingSlashes(issuer === "" ? DEFAULT_SIGNIN_ISSUER : issuer),
    clientId,
    clientSecret: secret.trim(),
  };
}
