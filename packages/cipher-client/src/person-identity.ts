// packages/cipher-client/src/person-identity.ts
//
// A person's own Google identity token as the subject of the broker's exchange, renewed from the
// refresh token their sign-in on this machine keeps. It mirrors the Go client's `PersonIdentity`
// and `DeviceFlow.Refresh` (`packages/envoy/internal/cipherclient/device.go`), with
// `openid-client` in the place of x/oauth2.

import { holdSecret, holdUrlUserinfo } from "@legion/contracts/held-secrets";
import { errorClass, urlForText } from "@legion/contracts/log-text";
import { callLibrary, LibraryError, refusalFetch } from "@legion/contracts/remote-text";
import {
  allowInsecureRequests,
  ClientSecretPost,
  Configuration,
  type CustomFetch,
  customFetch,
  discovery,
  refreshTokenGrant,
  type ServerMetadata,
  type TokenEndpointResponse,
} from "openid-client";
import { requestInit, statusOf } from "./broker-request";
import { endsGrant } from "./oauth-refusal";
import { REFUSAL_TEXT_WITHHELD, shownOAuthCode } from "./refusal-text";
import { SIGNIN_COMMAND, SIGNIN_ISSUER_VAR, type SignInFlow, type SignInRecord } from "./signins";
import type { SubjectToken, TokenSource } from "./source";
import { SignInEndedError } from "./token-errors";
import { SUBJECT_TOKEN_TYPE_PERSON } from "./wire";

/** Bounds each call to the issuer, as the Go client's `HTTPClient` bounds each of its calls
 *  (`callTimeout`, `packages/envoy/internal/cipherclient/environment.go`). In seconds, as
 *  `openid-client` takes it. */
const ISSUER_TIMEOUT_SECONDS = 30;

/** What {@link personIdentity} takes beside the sign-in and its client. */
export interface PersonIdentityOptions {
  /** The fetch every request to the issuer goes through; the global one by default. Only a test
   *  replaces it. */
  readonly fetch?: typeof fetch | undefined;
}

/**
 * What the issuer said in `error`: the status and OAuth code it answered, its description
 * withheld (`REFUSAL_TEXT_WITHHELD`), since an issuer, or anything at
 * AGENT_SECRETS_SIGNIN_ISSUER, can put the refresh token or the client secret it was sent there;
 * or the status and class of a failure that is none. `openid-client` reports a request that never
 * got an answer as "something went wrong" and hangs the reason off `cause`, so the direct cause's
 * class is named too, and no deeper one. A class, never a message: the library's and the
 * runtime's messages repeat what they were handed (a resolver's names the host, a parser's the
 * URL, a parser further down the body). Every error here crossed {@link callLibrary}, so its code
 * is {@link LibraryError}'s, compared as it came and shown only as the rule reads it.
 */
function issuerSaid(error: unknown): string {
  if (error instanceof LibraryError && error.answeredOAuthError) {
    const shown = error.code.isEmpty ? "" : shownOAuthCode(String(error.code));
    const code = shown === "" ? "" : ` ${shown}`;
    const withheld = error.description.isEmpty ? "" : `: ${REFUSAL_TEXT_WITHHELD}`;
    return `HTTP ${error.status}${code}${withheld}`;
  }
  let said = error instanceof LibraryError ? error.thrownClass : errorClass(error);
  const cause =
    error instanceof LibraryError
      ? error.causeClass
      : error instanceof Error && error.cause instanceof Error
        ? errorClass(error.cause)
        : "";
  if (cause !== "") said += `: ${cause}`;
  const status = statusOf(error);
  return status === 0 ? said : `HTTP ${status}: ${said}`;
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
 * The issuer's 400 or 401 `invalid_grant` ({@link endsGrant}) is a {@link SignInEndedError}, which
 * is final; every other failure is an ordinary error, which leaves a client's live token in hand.
 * An answer with no `id_token` is an error. A record made with a client other than `flow`'s is
 * refused here, before any request, in the words of Go's `registerSignInSession`
 * (`packages/envoy/cmd/cipher/login.go`), since no other client can renew it. The source keeps
 * only the record's person and refresh token, so the box key's PEM is not held for its life.
 */
export function personIdentity(
  record: SignInRecord,
  flow: SignInFlow,
  options: PersonIdentityOptions = {}
): TokenSource {
  const { person, refresh_token: refreshToken, client_id: clientId } = record;
  // Held, so every line this process prints is scrubbed of them: an issuer at the wrong URL can
  // echo what it was sent (`@legion/contracts/held-secrets`).
  holdSecret(refreshToken);
  holdSecret(flow.clientSecret);
  if (clientId !== "" && clientId !== flow.clientId) {
    throw new Error(
      `${person} signed in with another client than this machine now has (${clientId}), so their sign-in cannot be renewed; run: ${SIGNIN_COMMAND}`
    );
  }
  const send = options.fetch ?? fetch;
  const insecure = holdUrlUserinfo(flow.issuer).startsWith("http:");
  // Named as every client names a URL (`urlForText`); requests go to the issuer as configured.
  const issuer = urlForText(flow.issuer, SIGNIN_ISSUER_VAR);
  let discovered: Promise<ServerMetadata> | null = null;

  /** The fetch `openid-client` calls, bounded by its own timeout and by the caller's signal. */
  const through =
    (signal: AbortSignal | undefined): CustomFetch =>
    (url, init) =>
      send(url, requestInit(init, signal, init.headers));

  /** The issuer's metadata, as one discovery request reads it. */
  const discover = async (signal: AbortSignal | undefined): Promise<ServerMetadata> => {
    try {
      const config = await callLibrary(() =>
        discovery(new URL(flow.issuer), flow.clientId, undefined, undefined, {
          [customFetch]: refusalFetch(through(signal)),
          timeout: ISSUER_TIMEOUT_SECONDS,
          ...(insecure ? { execute: [allowInsecureRequests] } : {}),
        })
      );
      return callLibrary(() => config.serverMetadata());
    } catch (error) {
      // No `cause`: the library's error for an issuer that is not the one asked for holds the
      // issuer as configured, its user info included.
      throw new Error(`read the sign-in issuer ${issuer}: ${issuerSaid(error)}`);
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
        const config = callLibrary(
          () =>
            new Configuration(
              metadata,
              flow.clientId,
              undefined,
              ClientSecretPost(flow.clientSecret)
            )
        );
        if (insecure) callLibrary(() => allowInsecureRequests(config));
        config.timeout = ISSUER_TIMEOUT_SECONDS;
        config[customFetch] = refusalFetch(through(signal));
        answered = await callLibrary(() => refreshTokenGrant(config, refreshToken));
      } catch (error) {
        // No `cause` on either: an answer the library refused hangs off its error, and that answer
        // can hold the person's fresh ID token, which no log of this error may carry.
        if (endsGrant(error)) {
          throw new SignInEndedError(issuer, person);
        }
        throw new Error(`renew ${person}'s sign-in at ${issuer}: ${issuerSaid(error)}`);
      }
      const idToken = answered.id_token ?? "";
      if (idToken === "") {
        throw new Error(
          `${issuer} answered no id_token: the sign-in asked for the "openid" scope and must be answered one`
        );
      }
      return { token: idToken, tokenType: SUBJECT_TOKEN_TYPE_PERSON };
    },
    describe: () => `${person}'s sign-in on this machine`,
    person,
  };
}
