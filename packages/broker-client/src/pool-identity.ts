// packages/broker-client/src/pool-identity.ts
//
// The identity of a machine with no Google service account of its own, minted through a workload
// identity pool. It is a port of the Go client's `PoolIdentity`
// (`packages/envoy/internal/brokerclient/pool.go`), and asks the two services Go asks with the
// bodies Go sends, so a deployment that names a pool gets one identity from either client.

import { messageOf } from "./error-text";
import type { SubjectToken, TokenSource } from "./source";
import {
  GRANT_TYPE_TOKEN_EXCHANGE,
  SUBJECT_TOKEN_TYPE_GOOGLE,
  SUBJECT_TOKEN_TYPE_KUBERNETES,
} from "./wire";

/** Google's own security token service, where an external credential is traded for a federated
 *  access token: RFC 8693 again, in Google's spelling of it, a JSON body with camelCase members
 *  rather than a form (Go's `DefaultSecurityTokenServiceURL`). */
export const DEFAULT_SECURITY_TOKEN_SERVICE_URL = "https://sts.googleapis.com/v1/token";

/** Google's own IAM Credentials service, whose `generateIdToken` mints the identity token the
 *  broker reads (Go's `DefaultIAMCredentialsURL`). An origin: the resource path is built here. */
export const DEFAULT_IAM_CREDENTIALS_URL = "https://iamcredentials.googleapis.com";

/** What the security token service answers: a federated access token, spendable at IAM
 *  Credentials and nowhere else this source asks (Go's `poolRequestedTokenType`). */
const REQUESTED_TOKEN_TYPE = "urn:ietf:params:oauth:token-type:access_token";

/** The one scope a federated access token is asked for; IAM Credentials takes no narrower one
 *  (Go's `poolScope`). */
const SCOPE = "https://www.googleapis.com/auth/cloud-platform";

/** The `subjectTokenType` the security token service reads a Kubernetes projected token under. */
const SUBJECT_TOKEN_TYPE_JWT = "urn:ietf:params:oauth:token-type:jwt";

/** Bounds each of the two calls a mint makes (Go's `poolTimeout`). */
const POOL_TIMEOUT_MS = 20_000;

/** The most of an answer that is read, as Go's `io.LimitReader` bounds it, so a server in
 *  Google's place cannot grow this process without bound. */
const MAX_ANSWER_BYTES = 1 << 20;

/** What {@link poolIdentity} takes, member for member Go's `PoolIdentity`. */
export interface PoolIdentityOptions {
  /** The external credential the pool federates: the identity this machine already holds, such as
   *  a projected Kubernetes service-account token minted for {@link audience}. */
  readonly subject: TokenSource;
  /** The workload identity pool provider the external credential is presented to, the full
   *  resource name Google spells
   *  `//iam.googleapis.com/projects/<number>/locations/global/workloadIdentityPools/<pool>/providers/<provider>`. */
  readonly audience: string;
  /** The managed identity `generateIdToken` is called on: the service account the pool lets this
   *  credential act as, by its email address or its unique id. */
  readonly serviceAccount: string;
  /** The `aud` of the identity token `generateIdToken` mints: the client id the broker checks
   *  against `BROKER_GOOGLE_OIDC_AUDIENCE`. */
  readonly tokenAudience: string;
  /** {@link DEFAULT_SECURITY_TOKEN_SERVICE_URL} unless a dev stack or a test names a stand-in. */
  readonly securityTokenServiceUrl?: string | undefined;
  /** {@link DEFAULT_IAM_CREDENTIALS_URL} unless a dev stack or a test names a stand-in. */
  readonly iamCredentialsUrl?: string | undefined;
}

/**
 * The `subjectTokenType` the security token service reads an external credential under (Go's
 * `poolSubjectTokenType`). A Kubernetes projected token is a JWT there, whatever name the
 * broker's own exchange knows it by, since the pool provider that verifies it is an OIDC provider
 * and not this broker.
 */
function poolSubjectTokenType(brokerType: string): string {
  return brokerType === SUBJECT_TOKEN_TYPE_KUBERNETES ? SUBJECT_TOKEN_TYPE_JWT : brokerType;
}

/**
 * `segment` escaped as Go's `url.PathEscape` escapes a path segment, so both clients ask at one
 * path: an address's `@` stays as it is, and `/`, `?`, `;` and `,` are escaped.
 */
function pathEscape(segment: string): string {
  return encodeURIComponent(segment)
    .replace(/[!'()*]/g, (c) => `%${c.charCodeAt(0).toString(16).toUpperCase()}`)
    .replace(/%(?:24|26|2B|3A|3D|40)/g, decodeURIComponent);
}

/** The first {@link MAX_ANSWER_BYTES} of `answer`'s body, as text. */
async function answerText(answer: Response): Promise<string> {
  const reader = answer.body?.getReader();
  if (reader === undefined) return "";
  const chunks: Uint8Array[] = [];
  let read = 0;
  while (read < MAX_ANSWER_BYTES) {
    const { done, value } = await reader.read();
    if (done) break;
    const kept = value.subarray(0, MAX_ANSWER_BYTES - read);
    chunks.push(kept);
    read += kept.length;
  }
  await reader.cancel().catch(() => undefined);
  return Buffer.concat(chunks).toString("utf8");
}

/**
 * The identity of a machine with no Google service account of its own (Go's `PoolIdentity`): an
 * external credential, the subject this deployment already holds, traded at Google's security
 * token service for a federated access token, which IAM Credentials' `generateIdToken` then
 * spends to mint the Google identity token the broker reads. `includeEmail` is set on that mint,
 * because the broker looks a machine up by the token's `email` claim and a token minted without
 * it names no account at all.
 *
 * A token minted this way carries no `google.compute_engine` claims: it was minted off an
 * instance. An exchange entry that pins a project or an instance name refuses it, and one that
 * pins neither takes it, which is what the broker's own settings say.
 *
 * `read()` mints afresh at every call, and each of its two calls has a 20 s deadline. Every error
 * names the call that failed, with Google's status and reason where it answered one, and never a
 * token.
 */
export function poolIdentity(options: PoolIdentityOptions): TokenSource {
  const securityTokenService =
    options.securityTokenServiceUrl ?? DEFAULT_SECURITY_TOKEN_SERVICE_URL;
  const iamCredentials = (options.iamCredentialsUrl ?? DEFAULT_IAM_CREDENTIALS_URL).replace(
    /\/$/,
    ""
  );
  const account = pathEscape(options.serviceAccount);
  const mintEndpoint = `${iamCredentials}/v1/projects/-/serviceAccounts/${account}:generateIdToken`;

  /**
   * Posts `body` to `endpoint` as JSON, with `bearer` on the `Authorization` header where it is
   * not empty, and answers the member `member` of what came back, or "" where the answer holds
   * none. A non-2xx answer is an error naming the status and the body Google's services put their
   * reason in, which carries no credential of this machine's; a 2xx answer that is not JSON is
   * named without its text, since that text could be a token.
   */
  const call = async (
    endpoint: string,
    bearer: string,
    body: Record<string, unknown>,
    member: string,
    signal: AbortSignal | undefined
  ): Promise<string> => {
    const signals = [AbortSignal.timeout(POOL_TIMEOUT_MS), signal].filter(
      (one) => one !== undefined
    );
    const answer = await fetch(endpoint, {
      method: "POST",
      headers: {
        "Content-Type": "application/json",
        ...(bearer === "" ? {} : { Authorization: `Bearer ${bearer}` }),
      },
      body: JSON.stringify(body),
      signal: AbortSignal.any(signals),
    });
    const text = await answerText(answer);
    if (answer.status < 200 || answer.status > 299) {
      throw new Error(`HTTP ${answer.status}: ${text.trim()}`);
    }
    let parsed: unknown;
    try {
      parsed = JSON.parse(text);
    } catch {
      throw new Error(`HTTP ${answer.status}: the answer is not JSON`);
    }
    const value =
      typeof parsed === "object" && parsed !== null
        ? (parsed as Record<string, unknown>)[member]
        : undefined;
    return typeof value === "string" ? value : "";
  };

  /** The security token service's half: the external credential for a federated access token
   *  that acts as the pool's own principal (Go's `federate`). */
  const federate = async (subject: SubjectToken, signal: AbortSignal | undefined) => {
    let federated: string;
    try {
      federated = await call(
        securityTokenService,
        "",
        {
          audience: options.audience,
          grantType: GRANT_TYPE_TOKEN_EXCHANGE,
          requestedTokenType: REQUESTED_TOKEN_TYPE,
          scope: SCOPE,
          subjectTokenType: poolSubjectTokenType(subject.tokenType),
          subjectToken: subject.token,
        },
        "access_token",
        signal
      );
    } catch (error) {
      throw new Error(
        `trade this machine's credential at the security token service ${securityTokenService}: ${messageOf(error)}`,
        { cause: error }
      );
    }
    if (federated === "") {
      throw new Error(
        `the security token service ${securityTokenService} answered no access_token`
      );
    }
    return federated;
  };

  /** IAM Credentials' half: the federated access token spent on one `generateIdToken` for the
   *  managed identity, with `includeEmail` set (Go's `generateIDToken`). */
  const generateIdToken = async (federated: string, signal: AbortSignal | undefined) => {
    let token: string;
    try {
      token = await call(
        mintEndpoint,
        federated,
        { audience: options.tokenAudience, includeEmail: true },
        "token",
        signal
      );
    } catch (error) {
      throw new Error(
        `mint this machine's identity token at ${mintEndpoint}: ${messageOf(error)}`,
        { cause: error }
      );
    }
    if (token === "") throw new Error(`generateIdToken at ${mintEndpoint} answered no token`);
    return token;
  };

  return {
    async read(signal?: AbortSignal): Promise<SubjectToken> {
      let subject: SubjectToken;
      try {
        subject = await options.subject.read(signal);
      } catch (error) {
        throw new Error(
          `read the credential the workload identity pool federates: ${messageOf(error)}`,
          { cause: error }
        );
      }
      const federated = await federate(subject, signal);
      return {
        token: await generateIdToken(federated, signal),
        tokenType: SUBJECT_TOKEN_TYPE_GOOGLE,
      };
    },
    describe: () =>
      `a Google identity token for ${options.tokenAudience} minted through the workload identity pool ${options.audience} as ${options.serviceAccount}`,
  };
}
