// packages/broker-client/src/person-identity.ts
//
// A person's own Google identity token as the subject of the broker's exchange, renewed from the
// refresh token their sign-in on this machine keeps. It mirrors the Go client's `PersonIdentity`
// and `DeviceFlow.Refresh` (`packages/envoy/internal/brokerclient/device.go`), with
// `openid-client` in the place of x/oauth2.

import {
  allowInsecureRequests,
  ClientSecretPost,
  Configuration,
  type CustomFetch,
  customFetch,
  discovery,
  ResponseBodyError,
  refreshTokenGrant,
  type ServerMetadata,
  type TokenEndpointResponse,
} from "openid-client";
import { SIGNIN_COMMAND, type SignInFlow, type SignInRecord } from "./signins";
import type { SubjectToken, TokenSource } from "./source";
import { requestInit, SignInEndedError, statusOf } from "./token-client";
import { SUBJECT_TOKEN_TYPE_PERSON } from "./wire";

/** Bounds each call to the issuer, as the Go client's `HTTPClient` bounds each of its calls
 *  (`callTimeout`, `packages/envoy/internal/brokerclient/environment.go`). In seconds, as
 *  `openid-client` takes it. */
const ISSUER_TIMEOUT_SECONDS = 30;

/** What an issuer answers a refresh token it no longer honours (RFC 6749 Section 5.2). */
const INVALID_GRANT = "invalid_grant";

/** What {@link personIdentity} takes beside the sign-in and its client. */
export interface PersonIdentityOptions {
  /** The fetch every request to the issuer goes through; the global one by default. Only a test
   *  replaces it. */
  readonly fetch?: typeof fetch | undefined;
}

/**
 * What the issuer said in `error`: the OAuth error it answered, or the status and message of a
 * failure that is none. `openid-client` reports a request that never got an answer as "something
 * went wrong" and hangs the reason off `cause`, so the direct cause's message is named too, and
 * no deeper one: a parser's message further down can quote the answer's body.
 */
function issuerSaid(error: unknown): string {
  if (error instanceof ResponseBodyError) {
    const description = error.error_description ?? "";
    return `HTTP ${error.status} ${error.error}${description === "" ? "" : `: ${description}`}`;
  }
  let message = error instanceof Error ? error.message : String(error);
  const cause = error instanceof Error ? error.cause : undefined;
  if (cause instanceof Error && cause.message !== message) message += `: ${cause.message}`;
  const status = statusOf(error);
  return status === 0 ? message : `HTTP ${status}: ${message}`;
}

/**
 * The identity of the person `record` signed in, renewed through `flow`, the sign-in client the
 * deployment configures.
 *
 * `read()` spends the kept refresh token for a fresh ID token at every call, as Go's
 * `PersonIdentity.Token` does, so a sign-in that the person or Google has ended stops the client
 * at its next renewal rather than at the next hour. The issuer's discovery document is read once
 * per source, and read again only after a read of it failed. The refresh authenticates the
 * client in the form (`client_secret_post`, Go's `AuthStyleInParams`), and an `http` issuer, a
 * dev stack's, is posted to as it is named. A new refresh token in the answer is ignored, as Go
 * ignores it: the kept one stays the one to keep.
 *
 * The issuer's `invalid_grant` is a {@link SignInEndedError}, which is final; every other failure
 * is an ordinary error, which leaves a client's live token in hand. An answer with no `id_token`
 * is an error. A record made with a client other than `flow`'s is refused here, before any
 * request, in the words of Go's `registerSignInSession`
 * (`packages/envoy/cmd/agent-secrets/login.go`), since no other client can renew it.
 */
export function personIdentity(
  record: SignInRecord,
  flow: SignInFlow,
  options: PersonIdentityOptions = {}
): TokenSource {
  if (record.client_id !== "" && record.client_id !== flow.clientId) {
    throw new Error(
      `${record.person} signed in with another client than this machine now has (${record.client_id}), so their sign-in cannot be renewed; run: ${SIGNIN_COMMAND}`
    );
  }
  const send = options.fetch ?? fetch;
  const insecure = flow.issuer.startsWith("http:");
  let discovered: Promise<ServerMetadata> | null = null;

  /** The fetch `openid-client` calls, bounded by its own timeout and by the caller's signal. */
  const through =
    (signal: AbortSignal | undefined): CustomFetch =>
    (url, init) =>
      send(url, requestInit(init, signal, init.headers));

  /** The issuer's metadata, as one discovery request reads it. */
  const discover = async (signal: AbortSignal | undefined): Promise<ServerMetadata> => {
    try {
      const config = await discovery(new URL(flow.issuer), flow.clientId, undefined, undefined, {
        [customFetch]: through(signal),
        timeout: ISSUER_TIMEOUT_SECONDS,
        ...(insecure ? { execute: [allowInsecureRequests] } : {}),
      });
      return config.serverMetadata();
    } catch (error) {
      throw new Error(`read the sign-in issuer ${flow.issuer}: ${issuerSaid(error)}`, {
        cause: error,
      });
    }
  };

  return {
    async read(signal?: AbortSignal): Promise<SubjectToken> {
      discovered ??= discover(signal);
      const discovering = discovered;
      let metadata: ServerMetadata;
      try {
        metadata = await discovering;
      } catch (error) {
        // A discovery that failed is not kept, so the next read asks the issuer again.
        if (discovered === discovering) discovered = null;
        throw error;
      }
      let answered: TokenEndpointResponse;
      try {
        const config = new Configuration(
          metadata,
          flow.clientId,
          undefined,
          ClientSecretPost(flow.clientSecret)
        );
        if (insecure) allowInsecureRequests(config);
        config.timeout = ISSUER_TIMEOUT_SECONDS;
        config[customFetch] = through(signal);
        answered = await refreshTokenGrant(config, record.refresh_token);
      } catch (error) {
        if (error instanceof ResponseBodyError && error.error === INVALID_GRANT) {
          throw new SignInEndedError(flow.issuer, record.person, { cause: error });
        }
        // No `cause`: an answer the library refused hangs off its error, and that answer can hold
        // the person's fresh ID token, which no log of this error may carry.
        throw new Error(`renew ${record.person}'s sign-in at ${flow.issuer}: ${issuerSaid(error)}`);
      }
      const idToken = answered.id_token ?? "";
      if (idToken === "") {
        throw new Error(
          `${flow.issuer} answered no id_token: the sign-in asked for the "openid" scope and must be answered one`
        );
      }
      return { token: idToken, tokenType: SUBJECT_TOKEN_TYPE_PERSON };
    },
    describe: () => `${record.person}'s sign-in on this machine`,
  };
}
