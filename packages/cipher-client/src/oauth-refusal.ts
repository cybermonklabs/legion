// packages/cipher-client/src/oauth-refusal.ts
//
// The rules this package reads a token endpoint's answer by: the TypeScript statement of Go's
// `oauthrefusal` (`packages/envoy/internal/oauthrefusal`). The status alone decides whether the
// endpoint answered: a 5xx, a 408 and a 429 are a server that could not answer now, whatever their
// body names (`outage` of `@legion/contracts/http-status`, the workspace's one statement of that
// set, as Go's `oauthrefusal.Outage` is `httpoutage.Is`). An OAuth error code (RFC 6749 Section
// 5.2) that decides something counts only in a refusal, a 4xx that is no outage (`refusalCode`),
// and ending a grant is narrower still (`endsGrant`). A person's sign-in (`person-identity.ts`) and
// a machine's device login (`device-login.ts`) read their answers through these and `outage`, so
// each of these rules has one definition in this language; the broker's wording of a refusal
// (`brokerRefusal`, `broker-request.ts`) makes any 4xx OAuth error a `BrokerRefusedError`, as Go's
// broker client makes it a `RefusedError`, and leaves whether it is final to `outage`. The library
// under these reads fewer answers as an OAuth error than Go's x/oauth2 does: oauth4webapi does not
// read the body of an answer carrying `WWW-Authenticate`, and reads only a JSON body (oauth4webapi
// 3.8.8, build/index.js:914, :925-927), so an OAuth error in any other answer is never seen as one.

import { outage } from "@legion/contracts/http-status";
import { ResponseBodyError } from "openid-client";

/** What a token endpoint answers a refresh token or a code it no longer honours (RFC 6749
 *  Section 5.2). */
const INVALID_GRANT = "invalid_grant";

/**
 * The OAuth error code that `error` names where it is a token endpoint's refusal, a 4xx that is no
 * {@link outage}, and "" for every other failure, whatever error its body names: Go's
 * `oauthrefusal.Refusal`, which the Go helper's `pollLogin` reads a device code's poll by. RFC
 * 6749 Section 5.2 has an error answer be 400 "unless specified otherwise", and the broker's OAuth
 * library (authelia.com/provider/oauth2) specifies otherwise: it answers `access_denied` 403, the
 * denial of a device code's poll among them (RFC 8628 Section 3.5). A 5xx never reaches this as an
 * OAuth error: the library (oauth4webapi, under `openid-client`) reads an error body only from a
 * 4xx answer, and fails a 5xx as an answer that is not a token response.
 */
export function refusalCode(error: unknown): string {
  if (!(error instanceof ResponseBodyError)) return "";
  const refuses = error.status >= 400 && error.status < 500 && !outage(error.status);
  return refuses ? error.error : "";
}

/**
 * Whether `error` is a token endpoint's answer of 400 or 401 naming `invalid_grant` (RFC 6749
 * Section 5.2): the endpoint saying the refresh token or the code is expired, revoked or not the
 * client's, so the grant is over. It is the rule Go's `oauthrefusal.EndsGrant` states for ending a
 * grant: a person's sign-in the issuer ended (`SignInEndedError`), and a device login the broker
 * ended (`DeviceLoginEndedError`). It is narrower than {@link refusalCode} on purpose: ending a
 * grant is final, so it is drawn only from the statuses RFC 6749 Section 5.2 itself gives a
 * refusal, 400, and 401 for `invalid_client`. A 403 or a 429 naming `invalid_grant` is no such
 * refusal. A 5xx never reaches this as one: the library (oauth4webapi, under `openid-client`)
 * reads an error body only from a 4xx answer.
 */
export function endsGrant(error: unknown): boolean {
  return (
    error instanceof ResponseBodyError &&
    error.error === INVALID_GRANT &&
    (error.status === 400 || error.status === 401)
  );
}
