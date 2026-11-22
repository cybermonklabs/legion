import { holdSecret } from "@legion/contracts/held-secrets";
import {
  type CommandMode,
  type EnvoyConfigEnvironment,
  fileValue,
  httpUrl,
  nonEmpty,
} from "./envoy-config";
import { readSecretFile, sendable } from "./secret-file";

/** A bearer written in an envoy.json, and the server written beside it: the only server it may go
 * to. */
export interface FileBearer {
  readonly token: string;
  /** Where it is written: `dispatch.token` or `envoy.token`. */
  readonly field: string;
  /** The file it is written in. */
  readonly path: string;
  /** The server written beside it, or the section's default address when the file names none. */
  readonly server: string;
  /** A checkout's file, which the repository controls: a `!` value there is refused, never run. */
  readonly checkout: boolean;
}

/** Where a section's bearer comes from: a variable, which goes to whatever server resolves; a
 * file, whose bearer goes only to its own server; or nowhere. */
export type BearerSource =
  | { readonly kind: "variable"; readonly token: string }
  | ({ readonly kind: "file" } & FileBearer)
  | { readonly kind: "none" };

/** The section a bearer is for, which names its variables: `<SECTION>_URL`, `<SECTION>_TOKEN`,
 * `<SECTION>_TOKEN_FILE`. */
export type BearerSection = "DISPATCH" | "ENVOY";

/**
 * The scheme a credential is presented under. `Bearer` is a token whose holder is whoever has a
 * copy of it. `DPoP` (RFC 9449) is a token bound to a key the holder proves on every request,
 * which is what the secrets broker mints: a resource server refuses a DPoP-bound token presented
 * as a bearer (RFC 9449 Section 7.2), so the scheme belongs to the credential rather than being a
 * detail of how one request is written.
 */
export type AuthScheme = "Bearer" | "DPoP";

/**
 * A credential as one request presents it, which is how a caller holds one without knowing which
 * it is: a bearer written in an envoy.json or a variable (`bearerCredential`), or an access token
 * the secrets broker minted for a person signed in on this machine or for the machine's own
 * identity, with a fresh proof of the token's key for each request (`brokerCredential`,
 * src/broker-credential.ts).
 */
export interface RequestCredential {
  readonly scheme: AuthScheme;
  /**
   * The headers that authenticate `method` on `url`: `Authorization`, and for a key-bound scheme
   * the proof that goes with it. A broker credential trades for a token here when the one in hand
   * is about to lapse, and throws rather than answering headers it cannot make, so a caller that
   * writes only with what this gave it never writes with no identity.
   *
   * `session` is the session this one call acts for, where it acts for a session that is not
   * whichever one the caller holds now: the Envoy listener binds the session a broker token
   * names and answers 403 to a body naming another (`sessionNotBound`,
   * packages/envoy/cmd/listener/brokerauth.go), so a call that acts for a session must present
   * that session's own credential. A hand-back of the role a session this process has left still
   * holds is the call that needs it, and the listener binds the token's session on seven routes,
   * so every call whose body names the session it acts for passes that name (`transport.ts`). A
   * call that acts for no session in particular, and every read of another session's record,
   * passes nothing and is answered the credential the caller was built with. A bearer ignores it:
   * its holder is whoever has a copy of it.
   *
   * A bearer answers at once rather than with a promise, so a caller that must issue its request
   * before it does anything else still can: the Oh My Pi plugin's shutdown sends a session's
   * deregistration before its NATS connection starts draining, and one awaited microtask between
   * the two would reverse that. A caller that cares takes the answer as it comes and awaits only
   * a promise (`transport.ts`).
   *
   * `signal` is the call's own deadline. A broker credential fails fast, as gRPC's default call
   * does: where the token client's last trade failed in a way its retry loop trades again on and
   * nothing live is in hand, it throws `TradeRetryingError` at once. While the broker holds no
   * registration for this pod's session yet, it waits until `signal` ends and then throws
   * `RegistrationPendingError` (both `@legion/cipher-client/token-errors`), naming the session;
   * the loop goes on trading in the background, so a later call takes the token once the
   * registration lands. A caller that passes none waits as long as that takes. A bearer ignores it.
   */
  headers(
    method: string,
    url: string,
    session?: string,
    signal?: AbortSignal
  ): Readonly<Record<string, string>> | Promise<Readonly<Record<string, string>>>;
  /**
   * Tells the credential that the server said the token it presented for `session` (the same
   * argument `headers` takes, and nothing for the credential the caller was built with) is not
   * live, so that token is never presented again: Dispatch answers `401 BROKER_TOKEN_NOT_ACTIVE`
   * for a broker token the broker no longer holds live, such as one a person's revoke ended before
   * it lapsed, and a caller that is told so presents again once (`DispatchClient`'s `#send`).
   * `presented` is the very object of `headers`' answer the dead token went out in: where given,
   * the credential drops that token alone, from the session it was presented for, and keeps a
   * token it traded since and the token of a session its caller moved to in between. The next
   * `headers` presents the token the credential holds now, and trades where it holds none. A
   * broker credential has its client drop the token. A bearer has none: it holds no other token
   * to present.
   */
  stale?(session?: string, presented?: Readonly<Record<string, string>>): void;
  /**
   * The identity a presentation was made as, for `presented`, the very object one of `headers`'
   * answers was, where the credential can present as more than one: a broker credential follows
   * the sign-in this machine holds and the session its caller acts as, each of which can change
   * between two presentations. Dispatch keys a write per login, the person or the service
   * (`requestKeyLogin`, packages/envoy/internal/dispatch/api/request_keys.go), and a write that
   * may have landed is never sent again as another identity (`DispatchClient`'s `#write`). An
   * identity may be stricter than Dispatch's login: two identities can be one person, where the
   * credential cannot tell that they are. A credential that presents as one holder only, a
   * bearer, has none.
   */
  identity?(presented: Readonly<Record<string, string>>): string | undefined;
  /**
   * The secret this credential is presenting, which an excerpt of someone else's answer must
   * redact (`DispatchClient`'s `excerpt`), or "" while it holds none.
   */
  secret(): string;
}

/**
 * The signal a request's credential waits on: `deadline`, the request's own, which ends at
 * `deadlineMs`, less a reserve for the request itself, `min(1 s, deadlineMs / 2)`. For the
 * listener's 5 s attempt the credential waits 4 s and the request has 1 s; for a probe's shorter
 * deadline the two halve it. A broker credential whose wait ends with a live token in hand
 * answers with that token (`BrokerTokenClient.token`), so the request still has its reserve to be
 * sent and answered, rather than going out on a deadline the credential has already spent; one
 * whose wait ends with none leaves the request its reserve to go without it, which a listener that
 * asks for no credential answers.
 */
export function credentialDeadline(deadline: AbortSignal, deadlineMs: number): AbortSignal {
  const reserveMs = Math.min(1_000, deadlineMs / 2);
  return AbortSignal.any([deadline, AbortSignal.timeout(Math.ceil(deadlineMs - reserveMs))]);
}

/** A plain bearer: the same `Authorization` header on every request, and no proof. The token is
 *  held, so every line this process prints is scrubbed of it. */
export function bearerCredential(token: string): RequestCredential {
  holdSecret(token);
  return {
    scheme: "Bearer",
    headers: () => ({ Authorization: `Bearer ${token}` }),
    secret: () => token,
  };
}

/**
 * Where `section`'s bearer comes from: `<SECTION>_TOKEN_FILE` (the trimmed contents of the file it
 * names, how the Legion daemon delivers it to a pane), then `<SECTION>_TOKEN`, then the bearer
 * `written` gives, which is asked for only when neither variable is set. An empty variable counts
 * as unset. Throws naming the variable whose file cannot be read or whose value is not `sendable`,
 * never falling back to the next source, and whatever `written` throws.
 */
export function bearerSource(
  env: EnvoyConfigEnvironment,
  section: BearerSection,
  written: () => FileBearer | undefined
): BearerSource {
  const pointer = nonEmpty(env[`${section}_TOKEN_FILE`]);
  if (pointer !== undefined) {
    return { kind: "variable", token: readSecretFile(`${section}_TOKEN_FILE`, pointer) };
  }
  const plain = nonEmpty(env[`${section}_TOKEN`]);
  if (plain !== undefined) {
    if (!sendable(plain)) {
      throw new Error(`${section}_TOKEN holds whitespace or a control character`);
    }
    return { kind: "variable", token: plain };
  }
  const bearer = written();
  return bearer === undefined ? { kind: "none" } : { kind: "file", ...bearer };
}

/**
 * The bearer `bearer`'s file gives for `url`. Same server only: it goes to `url` only when that
 * names the origin of the server written beside it, which only `<SECTION>_URL` can make differ, and
 * is otherwise refused naming the variables that give another server its bearer. Then a checkout's
 * `!` value is refused, an empty value is refused, and the value is read (`fileValue`, which runs a
 * chosen file's `!` command and refuses whitespace inside a value; with `commands: "kept"` it only
 * takes the outcome a run already kept). Throws naming the field and the file, never the value.
 */
export function fileBearer(
  bearer: FileBearer,
  url: string,
  section: BearerSection,
  env: EnvoyConfigEnvironment,
  commands: CommandMode = "run"
): string {
  const named = httpUrl(url)?.origin;
  const paired = httpUrl(bearer.server)?.origin;
  if (named !== paired) {
    throw new Error(
      `${section}_URL names ${named}, but ${bearer.path}'s ${bearer.field} is for ${paired}; set ${section}_TOKEN or ${section}_TOKEN_FILE for it`
    );
  }
  if (bearer.checkout && bearer.token.startsWith("!")) {
    throw new Error(
      `${bearer.path}: ${bearer.field} is a command, which a checkout's envoy.json cannot run`
    );
  }
  if (bearer.token === "") {
    throw new Error(`${bearer.field} in ${bearer.path} must be a non-empty bearer token`);
  }
  return fileValue(bearer.token, bearer.field, bearer.path, env, commands);
}
