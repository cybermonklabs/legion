// packages/cipher-client/src/pool-identity.ts
//
// The identity of a machine with no Google service account of its own, minted through a workload
// identity pool. It is a port of the Go client's `PoolIdentity`
// (`packages/envoy/internal/cipherclient/pool.go`), and asks the two services Go asks with the
// bodies Go sends, so a deployment that names a pool gets one identity from either client.

import { holdUrlUserinfo } from "@legion/contracts/held-secrets";
import { errorClass, requestForText, urlForText } from "@legion/contracts/log-text";
import { joinUrl } from "@legion/contracts/url-join";
import { answerText } from "./answer-text";
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
 *  broker reads (Go's `DefaultIAMCredentialsURL`). An origin: the resource path is joined onto
 *  it here. */
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

/** What names each service in a text where its URL is not a plain http(s) URL (`urlForText`,
 *  `requestForText`): the option of `PoolIdentityOptions` that holds it. */
const STS_SETTING = "securityTokenServiceUrl";
const IAM_SETTING = "iamCredentialsUrl";

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
  /** The fetch both calls go through; the global one by default (Go's `HTTP`). Only a test
   *  replaces it. */
  readonly fetch?: typeof fetch | undefined;
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

/** A word as Google's services write their error codes: STS's OAuth `error` (`invalid_grant`)
 *  and IAM's `error.status` (`PERMISSION_DENIED`). */
const GOOGLE_ERROR_CODE = /^[A-Za-z_]{1,64}$/;

/**
 * ` <code>` for the error code a Google answer `text` carries (STS's `error`, IAM's
 * `error.status`), or "" where it carries none that reads as a code. The rest of the body is
 * never named: its text is the service's, and a service can echo the token it was sent.
 */
function googleErrorCode(text: string): string {
  let parsed: unknown;
  try {
    parsed = JSON.parse(text);
  } catch {
    return "";
  }
  const error =
    typeof parsed === "object" && parsed !== null
      ? (parsed as { error?: unknown }).error
      : undefined;
  const code =
    typeof error === "string"
      ? error
      : typeof error === "object" && error !== null
        ? (error as { status?: unknown }).status
        : undefined;
  return typeof code === "string" && GOOGLE_ERROR_CODE.test(code) ? ` ${code}` : "";
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
 * token. A service is named by the shared rule (`urlForText`; the mint as a request built on IAM
 * Credentials' base, `requestForText`), a request that got no answer by its error's class
 * (`errorClass`), and no error keeps another as its cause. The mint's resource is joined onto IAM
 * Credentials' URL, its path and query kept (`joinUrl`, Go's `urljoin.Join`); a URL that does not
 * parse fails the mint as "not a valid URL", as Go's does.
 */
export function poolIdentity(options: PoolIdentityOptions): TokenSource {
  const securityTokenService = holdUrlUserinfo(
    options.securityTokenServiceUrl ?? DEFAULT_SECURITY_TOKEN_SERVICE_URL
  );
  const iamCredentials = holdUrlUserinfo(options.iamCredentialsUrl ?? DEFAULT_IAM_CREDENTIALS_URL);
  const mintPath = `v1/projects/-/serviceAccounts/${pathEscape(options.serviceAccount)}:generateIdToken`;
  const securityTokenServiceShown = urlForText(securityTokenService, STS_SETTING);
  const send = options.fetch ?? fetch;

  /**
   * Posts `body` to `endpoint` as JSON, with `bearer` on the `Authorization` header where it is
   * not empty, and answers the member `member` of what came back, or "" where the answer holds
   * none. A non-2xx answer is an error naming the status and Google's error code alone
   * (`googleErrorCode`), never the body, which a service, or anything at its URL, can fill with
   * the token it was sent; a 2xx answer that is not JSON is
   * named without its text, since that text could be a token; and a request that got no answer is
   * named by its error's class, since the runtime's message can repeat the host it was handed. No
   * redirect is followed: Google's services answer none, and a 307 or 308 would send the body, a
   * token in it, on to another origin, so a redirect is a non-2xx answer like any other.
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
    let answer: Response;
    let text: string;
    try {
      answer = await send(endpoint, {
        method: "POST",
        headers: {
          "Content-Type": "application/json",
          ...(bearer === "" ? {} : { Authorization: `Bearer ${bearer}` }),
        },
        body: JSON.stringify(body),
        signal: AbortSignal.any(signals),
        redirect: "manual",
      });
      text = await answerText(answer);
    } catch (error) {
      throw new Error(`no answer: ${errorClass(error)}`);
    }
    if (answer.status < 200 || answer.status > 299) {
      throw new Error(`HTTP ${answer.status}${googleErrorCode(text)}`);
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
      // No `cause`: the error it would hold can carry the security token service's answer, and so
      // a token, which no log of this error may carry.
      throw new Error(
        `trade this machine's credential at the security token service ${securityTokenServiceShown}: ${messageOf(error)}`
      );
    }
    if (federated === "") {
      throw new Error(
        `the security token service ${securityTokenServiceShown} answered no access_token`
      );
    }
    return federated;
  };

  /** IAM Credentials' half: the federated access token spent on one `generateIdToken` for the
   *  managed identity, with `includeEmail` set (Go's `generateIDToken`). */
  const generateIdToken = async (federated: string, signal: AbortSignal | undefined) => {
    let mintEndpoint: string;
    try {
      mintEndpoint = joinUrl(iamCredentials, mintPath).href;
    } catch {
      // joinUrl's error quotes neither text; the URL is named as the shared rule names it.
      throw new Error(
        `mint this machine's identity token at ${urlForText(iamCredentials, IAM_SETTING)}: not a valid URL`
      );
    }
    const mintEndpointShown = requestForText(iamCredentials, mintEndpoint, IAM_SETTING);
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
      // No `cause`: every text of the call's is its own (`call`), and the error it would hold can
      // carry IAM Credentials' answer.
      throw new Error(
        `mint this machine's identity token at ${mintEndpointShown}: ${messageOf(error)}`
      );
    }
    if (token === "") throw new Error(`generateIdToken at ${mintEndpointShown} answered no token`);
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
