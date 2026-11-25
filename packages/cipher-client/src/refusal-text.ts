// packages/cipher-client/src/refusal-text.ts
//
// What this package prints of a refusal the broker or an issuer answered: the broker's own
// sentences it prints as written (`isKnownBrokerRefusal`), the stand-in for any other text
// (`REFUSAL_TEXT_WITHHELD`), and the OAuth codes it shows (`shownOAuthCode`). A refusal's text is
// a peer's choice of characters, so it prints only where it is one of these. The token client's
// errors (`token-errors.ts`, which its broker requests in `broker-request.ts` throw) and the
// person-identity client (`person-identity.ts`) read this policy; the corpus the broker's own
// library writes (packages/envoy/internal/cipher/oauth/testdata) holds it to what the broker
// sends.

import { POD_SESSION_NOT_REGISTERED, SESSION_NAME_NOT_THIS_REGISTRARS } from "./wire";

/**
 * The refusal texts of the broker's this client prints as the broker wrote them: the ones the
 * shared corpus pins (`packages/envoy/internal/cipherclient/testdata/wire.json`, which the Go
 * client reads too), each fixed words naming no token. Any other `error_description` is a peer's
 * free text: a broker, or anything at `AGENT_SECRETS_URL`, can put there the subject token it was
 * sent, so it is withheld, as nats-server's text other than its fixed refusals is
 * (`@legion/envoy-client/errors`). The status and the OAuth code still say what was refused, and
 * the broker's own log keeps its text.
 */
export const KNOWN_BROKER_REFUSALS: ReadonlySet<string> = new Set([
  "no entry lists the account this token names",
  "too many token requests; try again later",
  "the proof's iat is outside the broker's skew window",
]);

/** A person as the broker's sentences name one: an email address. Where the trade is a person's
 *  sign-in it must be that person (`TokenSource.person`); a machine's trade has no person to
 *  compare, and the slot is bounded. */
const PERSON_SLOT =
  "(?<person>[A-Za-z0-9._+-]{1,64}@[A-Za-z0-9-]{1,63}(?:\\.[A-Za-z0-9-]{1,63}){1,3})";
/** A project key as Dispatch writes one (`DSP`, `INFRA`). */
const PROJECT_SLOT = "[A-Z][A-Z0-9]{1,15}";
/** A session name, which must be the one this client traded under. */
const SESSION_SLOT = "(?<session>[^'\\s]{1,128})";

/** The other slots the exchange policy's sentences carry, each the shape of what fills it. */
const EMAIL = "[A-Za-z0-9._+-]{1,64}@[A-Za-z0-9-]{1,63}(?:\\.[A-Za-z0-9-]{1,63}){1,3}";
const K8S_ACCOUNT = "system:serviceaccount:[a-z0-9][a-z0-9.-]{0,62}:[a-z0-9][a-z0-9.-]{0,252}";
const REPOSITORY = "[A-Za-z0-9_.-]{1,100}/[A-Za-z0-9_.-]{1,100}";
const WORKFLOW_PATH = "[A-Za-z0-9_./-]{1,200}";
const GIT_REF = "refs/[A-Za-z0-9_./-]{1,200}";
const WORKFLOW_REF = "[A-Za-z0-9_./@-]{1,400}";
const IDENTITY = `(?:${K8S_ACCOUNT}|${REPOSITORY}/${WORKFLOW_PATH}|${EMAIL})`;
const FIXED_SESSION = "[A-Za-z0-9._:@/+-]{1,200}";
const PROJECT_LIST = `${PROJECT_SLOT}(?: ${PROJECT_SLOT}){0,31}`;
const SERVICE = "[A-Za-z0-9._-]{1,100}";
const CLOUD_PROJECT = "[a-z][a-z0-9-]{4,29}";
const INSTANCE = "[a-z0-9][a-z0-9-]{0,62}";
const INSTANCE_PATTERN = "[A-Za-z0-9*?._\\[\\]-]{1,100}";
const NUMBER = "[0-9]{1,6}";
const CLAIM = "[A-Za-z0-9_.:-]{1,64}";
const URI = "[A-Za-z0-9:/._-]{1,200}";
/** The exchange policy's one sentence of what a session name may be (Go's `sessionRule`). */
const SESSION_RULE =
  "a session name is printable, carries no whitespace, '\\*' or '>', and is at most [0-9]{1,4} bytes";

function escapeRegExp(text: string): string {
  return text.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
}

/** A Go format's literal words, escaped, with each `%s` or `%d` replaced by its slot in order. */
function formatPattern(format: string, slots: readonly string[]): string {
  const words = format.split(/%[sd]/);
  if (words.length !== slots.length + 1) {
    throw new Error(`the format ${JSON.stringify(format)} takes ${words.length - 1} slots`);
  }
  return words.map((word, at) => escapeRegExp(word) + (slots[at] ?? "")).join("");
}

/** The exchange policy's sentences (`packages/envoy/internal/cipher/exchange`), by the sentinel
 *  each wraps, as the broker hands them on: `The subject token was refused: <sentinel>: <detail>.` */
const POLICY_SENTINELS = {
  noEntry: "exchange: no entry lists this identity",
  noPodBinding: "exchange: the subject token is bound to no pod",
  sessionMismatch: "exchange: the request names a session the entry does not fix",
  sessionRequired: "exchange: the request names no session and the entry fixes none",
  sessionInvalid: "exchange: the request names an unusable session",
  claimMissing: "exchange: the subject token carries no such claim",
  emailNotVerified: "exchange: the subject token's email address is not verified",
  notInDomain: "exchange: the person's Google account is not in the sign-in domain",
  noActorToken: "exchange: the person's token came with no machine acting for them",
  noComputeEngine: "exchange: the subject token carries no Compute Engine claims",
  machineNotAllowed: "exchange: the machine is not one the entry allows",
  sessionFixed: "exchange: the entry fixes the session name its tokens carry",
  projectsFixed: "exchange: the request names projects other than the one its entry gives",
  projectsNoPerson: "exchange: a token that names no person names no project",
} as const;

function policy(sentinel: keyof typeof POLICY_SENTINELS, format: string, ...slots: string[]) {
  return `${escapeRegExp(`The subject token was refused: ${POLICY_SENTINELS[sentinel]}: `)}${formatPattern(format, slots)}\\.`;
}

/**
 * The exchange policy's own refusals of a subject token, each the Go format it is written with and
 * the shape of each slot, in place of an open "The subject token was refused: <anything>" pattern,
 * which let a token cut into pieces through. The refusal that quotes an error text of its own
 * (`not a person's Google identity token (%v)`) is left out, so it is withheld.
 */
const POLICY_REFUSAL_PATTERNS: readonly string[] = [
  policy(
    "noPodBinding",
    "%s presented a token bound to no pod, which proves only that the account exists",
    K8S_ACCOUNT
  ),
  policy("noEntry", "no entry lists the service account %s", K8S_ACCOUNT),
  policy(
    "sessionFixed",
    "%s's tokens all name the session %s, which every pod of it may use, so its pods are registered for no name",
    K8S_ACCOUNT,
    FIXED_SESSION
  ),
  policy(
    "sessionInvalid",
    "the registration of %s names a session of %d bytes; %s",
    K8S_ACCOUNT,
    NUMBER,
    SESSION_RULE
  ),
  policy(
    "noEntry",
    "%s ran %s, whose job is written in %s; an entry lists the workflow a job runs in",
    REPOSITORY,
    WORKFLOW_REF,
    WORKFLOW_REF
  ),
  policy(
    "claimMissing",
    "the subject token's workflow_ref is not <owner>/<name>/<path>@<ref> under its repository %s",
    REPOSITORY
  ),
  policy(
    "noEntry",
    "no entry lists the workflow %s of %s on %s",
    WORKFLOW_PATH,
    REPOSITORY,
    GIT_REF
  ),
  policy(
    "claimMissing",
    "the subject token carries no email claim, so it names no account (ask for it with format=full)"
  ),
  policy("noEntry", "no entry lists the Google account %s", EMAIL),
  policy("emailNotVerified", "%s presented a token whose email_verified is not true", EMAIL),
  policy(
    "projectsNoPerson",
    "%s is the service %s and names no person, and the request named %s",
    EMAIL,
    SERVICE,
    PROJECT_LIST
  ),
  policy(
    "projectsNoPerson",
    "%s reads as the service %s and names no person, and the request named %s",
    K8S_ACCOUNT,
    SERVICE,
    PROJECT_LIST
  ),
  policy(
    "noComputeEngine",
    "%s presented a token carrying no Compute Engine identity, and its entry takes machines of the project %s named %s",
    EMAIL,
    CLOUD_PROJECT,
    INSTANCE_PATTERN
  ),
  policy(
    "machineNotAllowed",
    "%s presented a token minted in the project %s, and its entry takes machines of %s",
    EMAIL,
    CLOUD_PROJECT,
    CLOUD_PROJECT
  ),
  policy(
    "machineNotAllowed",
    "%s presented a token minted on the instance %s, and its entry takes the names %s",
    EMAIL,
    INSTANCE,
    INSTANCE_PATTERN
  ),
  policy(
    "noActorToken",
    "a person signs in on a machine, so the machine's own Google identity token is presented as the actor token (%s)",
    URI
  ),
  policy(
    "claimMissing",
    "the actor token carries no email claim, so it names no machine (ask for it with format=full)"
  ),
  policy(
    "noEntry",
    "no entry lists the Google account %s, which the actor token names as the machine",
    EMAIL
  ),
  policy(
    "emailNotVerified",
    "the machine %s presented an actor token whose email_verified is not true",
    EMAIL
  ),
  policy("claimMissing", "the subject token carries no sub claim, so it names no person"),
  policy(
    "claimMissing",
    "the subject token carries no email claim of an address's shape, so it names no person"
  ),
  policy(
    "notInDomain",
    "%s signed in, and this broker takes the Google Workspace domain BROKER_SIGNIN_DOMAIN names; sign in again with `agent-secrets login` as an account of that domain",
    PERSON_SLOT
  ),
  policy(
    "claimMissing",
    "the subject token names no machine credential id, so it is not a machine login's token"
  ),
  policy("claimMissing", "the machine credential records no approver to act for"),
  policy("sessionMismatch", "%s's tokens name the session %s", IDENTITY, FIXED_SESSION),
  policy(
    "sessionRequired",
    "%s is listed with no session of its own, so the request names one",
    IDENTITY
  ),
  policy("sessionInvalid", "%s named a session of %d bytes; %s", IDENTITY, NUMBER, SESSION_RULE),
  policy("claimMissing", "the subject token carries no %s claim", CLAIM),
  policy("claimMissing", "the subject token's %s claim is not a non-empty string", CLAIM),
  policy(
    "projectsFixed",
    "%s's entry names no project, and the request named %s",
    IDENTITY,
    PROJECT_LIST
  ),
  policy(
    "projectsFixed",
    "%s writes the project %s that its entry names, and the request named %s",
    IDENTITY,
    PROJECT_SLOT,
    PROJECT_LIST
  ),
  // The broker's own audience check (`oauth/exchange.go`, `audience`), which wraps no sentinel.
  `${escapeRegExp("The subject token was refused: ")}${formatPattern("the subject token was not minted for %s, which is the audience this broker accepts from %s", [URI, URI])}\\.`,
];
/**
 * The broker's refusal sentences (`packages/envoy/internal/cipher/oauth/exchange.go`'s hints),
 * as whole-text patterns. Their slots take a person's email, which must be the signed-in person's
 * where the trade is a sign-in, a project key, or a session name, which must be the one this
 * client traded under. These carry the remedy a person acts on (`Sign in again with
 * agent-secrets login`, `set AGENT_SECRETS_PROJECTS`, `Ask under a name of your own`).
 * `refusal-descriptions.json` holds each as the broker's own library writes it.
 */
const KNOWN_BROKER_REFUSAL_PATTERNS: readonly RegExp[] = [
  `${PERSON_SLOT} revoked this machine's sign-in, so the key this proof was signed with trades nothing\\. Sign in again with \`agent-secrets login\`\\.`,
  `${PERSON_SLOT}'s sign-in on this box key was made on another machine, and a sign-in belongs to the machine it was made on\\. Sign in again with \`agent-secrets login\`\\.`,
  "The machine login the subject token belongs to has ended\\.",
  "The machine login has ended\\.",
  "Another live machine login holds the key this proof was signed with\\. A (?:machine makes a key of its own|sign-in makes a box key of its own)\\.",
  // The same two trades refused a key a live read token is bound to.
  "A live read token is bound to the key this proof was signed with\\. Make a new key\\.",
  `${PERSON_SLOT} is a restricted member, whose token names its projects: set AGENT_SECRETS_PROJECTS to the projects this session works on\\.`,
  `The token names a project its person cannot see: ${PROJECT_SLOT}\\.`,
  `The subject token was refused: its entry names the project ${PROJECT_SLOT}, which its person ${PERSON_SLOT} cannot see\\.`,
  `The subject token was refused: its entry names no project, and its person ${PERSON_SLOT} is a restricted member, who sees only the projects that name them\\.`,
  "Dispatch could not be asked whether the token's person sees the projects it names; try again\\.",
  `The session name '${SESSION_SLOT}' belongs to another identity: one the broker's configuration fixes it for, or one that has written under it within the last [0-9]{1,4} days\\. Ask under a name of your own\\.`,
  `No registration says this pod may write under the session name '${SESSION_SLOT}'\\. A pod writes under a session name its deployment registered for it\\.`,
  `Refused: this pod's registration for session '${SESSION_SLOT}' is bound to another key; a role trades only for its own session\\.`,
  `Refused: this pod's registration for session '${SESSION_SLOT}' was released; a pod trades only under a registration its registrar has not released\\.`,
  `The session name '${SESSION_SLOT}' belongs to another daemon: a pod trades only under a session name the daemon that registered it holds\\.`,
  `The session name '${SESSION_SLOT}' is bound to no daemon: a pod trades only under a session name the daemon that registered it holds\\.`,
  // Dispatch's hold of a person (`membership_held`), as the wire corpus's own sentence writes it:
  // the same person each time.
  `${PERSON_SLOT}'s credentials are held by Dispatch; they work again when \\k<person> signs in to Dispatch, or when Dispatch can confirm \\k<person> with Google again`,
  // A read trade's own refusals: the cut (`as_of`), the launcher credential a read trade never
  // makes or moves, the holder's proof (`DPoP-Holder`), and the key a read token may not share
  // with a launcher credential.
  "The 'as_of' parameter names a cut, and only a trade that asks for the scope read may name one\\.",
  "The 'as_of' parameter is not an RFC 3339 time with at most six fractional digits\\.",
  "The 'as_of' parameter names the zero time, which is no cut: a trade that reads the present names no cut\\.",
  "The 'as_of' parameter names a time later than this trade, and a cut is a time that has passed\\.",
  "A read trade makes or moves no launcher credential, so it cannot send launcher_credential=true\\. Trade for read without the parameter\\.",
  `${PERSON_SLOT}'s sign-in on this machine holds no launcher credential yet, and a read trade makes none\\. Sign in with \`agent-secrets login\` first\\.`,
  `${PERSON_SLOT}'s sign-in on this machine holds a launcher credential that has lapsed, and a read trade renews none\\. One ordinary trade of the sign-in renews it, as \`agent-secrets\` makes; then trade for read again\\.`,
  "A person's read trade sends a DPoP-Holder proof: the box key proves the sign-in and is bound to no read token, so the token is bound to a key made for its holder\\.",
  "The DPoP-Holder header binds a read token to its holder's key, and this trade does not ask for the scope read\\. Ask for it, or send no DPoP-Holder header\\.",
  "The DPoP-Holder proof was refused: a trade sends one, a DPoP proof of this request signed with the key it embeds, spent nowhere before\\.",
  "The DPoP-Holder proof is signed with the key the DPoP proof is signed with, and the holder's key is a key of its own\\.",
  "The key this read token would be bound to signs launcher proofs: a live launcher credential is bound to it, and no read token is bound to such a key\\. Send a DPoP-Holder proof by a key made for the token's holder, and the token is bound to that key\\.",
  "The key this proof was signed with signs launcher proofs: a live launcher credential is bound to it, and no read token is bound to such a key\\. Ask for the scope read with a DPoP-Holder proof by a key made for the token's holder, or trade with a key that holds no launcher credential\\.",
  ...POLICY_REFUSAL_PATTERNS,
].map((source) => new RegExp(`^${source}$`));

/**
 * The fixed sentence the broker's OAuth library (authelia.com/provider/oauth2, an RFC6749Error's
 * description) writes ahead of each hint, by error code, and the `<CODE>: ` the broker's own
 * refusals (`oauth.WriteRefusal`) write ahead of their message.
 */
const LIBRARY_DESCRIPTIONS: readonly string[] = [
  "The provided authorization grant (e.g., authorization code, resource owner credentials) or refresh token is invalid, expired, revoked, does not match the redirection URI used in the authorization request, or was issued to another client.",
  "The request is missing a required parameter, includes an invalid parameter value, includes a parameter more than once, or is otherwise malformed.",
  "The authorization server is currently unable to handle the request due to a temporary overloading or maintenance of the server.",
  "The authorization server encountered an unexpected condition that prevented it from fulfilling the request.",
];
const REFUSAL_CODE_PREFIX = /^[A-Z][A-Z_]{1,63}: /;
/** The prefixes the broker writes in place of the library's sentence: a code and a colon. */
const CODE_PREFIXES = [POD_SESSION_NOT_REGISTERED, SESSION_NAME_NOT_THIS_REGISTRARS].map(
  (code) => `${code}: `
);

/** `text` with the library's fixed sentence, the broker's refusal code, or one of
 *  {@link CODE_PREFIXES}, taken off its front. */
function brokerHint(text: string): string {
  for (const fixed of LIBRARY_DESCRIPTIONS) {
    if (text.startsWith(`${fixed} `)) return text.slice(fixed.length + 1);
  }
  for (const prefix of CODE_PREFIXES) {
    if (text.startsWith(prefix)) return text.slice(prefix.length);
  }
  return text.replace(REFUSAL_CODE_PREFIX, "");
}

/** What a trade sent that a refusal's slots may name: the signed-in person, where the trade is a
 *  sign-in, and the session name. */
export interface RefusalContext {
  readonly person?: string;
  readonly session?: string;
}

/** Whether `text` is one of the broker's known refusals, as its library writes it: one the corpus
 *  pins ({@link KNOWN_BROKER_REFUSALS}), or one of its sentences whose slots hold only what
 *  `context` allows. The first sentence that matches decides. */
export function isKnownBrokerRefusal(text: string, context: RefusalContext = {}): boolean {
  const hint = brokerHint(text);
  if (KNOWN_BROKER_REFUSALS.has(hint)) return true;
  for (const pattern of KNOWN_BROKER_REFUSAL_PATTERNS) {
    const matched = pattern.exec(hint);
    if (matched === null) continue;
    const { person, session } = matched.groups ?? {};
    if (session !== undefined && session !== context.session) return false;
    if (person !== undefined && context.person !== undefined)
      return person.toLowerCase() === context.person.toLowerCase();
    return true;
  }
  return false;
}

/** What stands in for a refusal text that is not one of {@link KNOWN_BROKER_REFUSALS}. */
export const REFUSAL_TEXT_WITHHELD = "(its text is withheld)";

/**
 * The OAuth error codes of the broker's library the clients handle: RFC 6749's token-endpoint
 * codes (Section 5.2: invalid_request, invalid_client, invalid_grant, unauthorized_client,
 * unsupported_grant_type, invalid_scope) and its two an authorization server answers when it
 * cannot serve (Section 4.1.2.1: server_error, temporarily_unavailable, and access_denied, which
 * an issuer answers a refused grant with), RFC 8693's invalid_target (Section 2.2.2), and RFC
 * 9449's invalid_dpop_proof and use_dpop_nonce (Sections 5 and 8). The broker's library writes
 * each of these (`packages/envoy/internal/cipher/oauth/testdata/refusal-codes.json`, which the Go
 * refusal corpus writes from the library's own errors).
 */
export const LIBRARY_OAUTH_ERROR_CODES: ReadonlySet<string> = new Set([
  "invalid_request",
  "invalid_client",
  "invalid_grant",
  "unauthorized_client",
  "unsupported_grant_type",
  "invalid_scope",
  "access_denied",
  "server_error",
  "temporarily_unavailable",
  "invalid_target",
  "invalid_dpop_proof",
  "use_dpop_nonce",
]);

/** The broker's own OAuth error codes, beside its library's: `membership_held`
 *  (`oauthwire.MembershipHeld`), a person Dispatch holds, which a client trades again on. The
 *  wire corpus (`packages/envoy/internal/cipherclient/testdata/wire.json`) answers each. */
export const BROKER_OAUTH_ERROR_CODES: ReadonlySet<string> = new Set(["membership_held"]);

/**
 * The OAuth error codes the clients handle, the only ones a text shows: the library's
 * ({@link LIBRARY_OAUTH_ERROR_CODES}) and the broker's own ({@link BROKER_OAUTH_ERROR_CODES}). The
 * broker's other codes of its own (`RATE_LIMITED`) go in its text, not here. A code is a peer's
 * choice of characters, which can be a token the client sent it, lowercased past the
 * case-sensitive scrub, so no other code is ever shown.
 */
export const KNOWN_OAUTH_ERROR_CODES: ReadonlySet<string> = new Set([
  ...LIBRARY_OAUTH_ERROR_CODES,
  ...BROKER_OAUTH_ERROR_CODES,
]);

/** `code` as a text shows it: one of {@link KNOWN_OAUTH_ERROR_CODES} as itself, "" for none, and
 *  any other as `unrecognised error code (<N> chars)`, which names none of its characters. */
export function shownOAuthCode(code: string): string {
  if (code === "") return "";
  return KNOWN_OAUTH_ERROR_CODES.has(code)
    ? code
    : `unrecognised error code (${code.length} chars)`;
}
