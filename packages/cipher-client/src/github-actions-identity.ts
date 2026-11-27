// packages/cipher-client/src/github-actions-identity.ts
//
// The identity of a GitHub Actions run: the OIDC token GitHub mints for the job. It is a port of
// the Go client's `GitHubActionsIdentity` (`packages/envoy/internal/cipherclient/githubactions.go`),
// and asks GitHub as Go asks, so a job that names this identity gets one token from either client.

import { holdSecret, holdUrlUserinfo } from "@legion/contracts/held-secrets";
import { errorClass, isCleanUrl } from "@legion/contracts/log-text";
import { answerText } from "./answer-text";
import { LOOPBACK_HOST } from "./loopback";
import { MACHINE_AUDIENCE_VAR } from "./machine-audience";
import type { SubjectToken, TokenSource } from "./source";
import { SUBJECT_TOKEN_TYPE_GITHUB } from "./wire";

/** Where a GitHub Actions job asks GitHub for its OIDC token, which GitHub sets in a job that
 *  holds `id-token: write` (Go's `ActionsIDTokenRequestURLVar`). */
export const ACTIONS_ID_TOKEN_REQUEST_URL_VAR = "ACTIONS_ID_TOKEN_REQUEST_URL";

/** The bearer that job asks with, which GitHub sets beside it (Go's
 *  `ActionsIDTokenRequestTokenVar`). */
export const ACTIONS_ID_TOKEN_REQUEST_TOKEN_VAR = "ACTIONS_ID_TOKEN_REQUEST_TOKEN";

/** Bounds one request for a run's token (Go's `githubActionsTimeout`). */
const TIMEOUT_MS = 20_000;

/** What begins every refusal of a request for the run's token. */
const ASKING = `ask GitHub for this run's OIDC token at the URL ${ACTIONS_ID_TOKEN_REQUEST_URL_VAR} names`;

/** The refusal of a request URL the request token may not be sent to. */
const NOT_HTTPS = `${ACTIONS_ID_TOKEN_REQUEST_URL_VAR} is not an https URL: the request token is sent there, so it takes https, or http to 127.0.0.1, ::1 or localhost`;

/** What {@link githubActionsIdentity} takes: Go's `GitHubActionsIdentity`'s members. */
export interface GitHubActionsIdentityOptions {
  /** Where the job asks for its token: {@link ACTIONS_ID_TOKEN_REQUEST_URL_VAR}'s value. */
  readonly requestUrl: string;
  /** The bearer the job asks with: {@link ACTIONS_ID_TOKEN_REQUEST_TOKEN_VAR}'s value. */
  readonly requestToken: string;
  /** The `aud` GitHub mints the token for, the broker's exchange audience:
   *  {@link MACHINE_AUDIENCE_VAR}'s value, which `describe` names. */
  readonly audience: string;
  /** The fetch the request goes through; the global one by default (Go's `HTTP`). Only a test
   *  replaces it. */
  readonly fetch?: typeof fetch | undefined;
}

/** `requestUrl` with `audience` added to its own query, refused, unquoted, where it is no https
 *  URL and no http URL of this machine's loopback. */
function requestEndpoint(requestUrl: string, audience: string): string {
  let endpoint: URL;
  try {
    endpoint = new URL(requestUrl);
  } catch {
    throw new Error(NOT_HTTPS);
  }
  const loopback = endpoint.protocol === "http:" && LOOPBACK_HOST.test(endpoint.hostname);
  if (endpoint.protocol !== "https:" && !loopback) throw new Error(NOT_HTTPS);
  endpoint.searchParams.set("audience", audience);
  return endpoint.href;
}

/**
 * The identity of a GitHub Actions run (Go's `GitHubActionsIdentity`): the OIDC token GitHub mints
 * for the job for `audience`, asked for at the request URL GitHub gave the job with the request
 * token it gave it. The broker's exchange reads the token as a workflow run's
 * ({@link SUBJECT_TOKEN_TYPE_GITHUB}) and answers it from that workflow's entry.
 *
 * The broker takes each GitHub token once, so `read()` asks GitHub at every call and keeps
 * nothing. The request adds the audience to the request URL's own query, which already holds
 * GitHub's `api-version`, and sends the request token as its bearer. It follows no redirect, and
 * it is never sent to a request URL that is not https, except on this machine's loopback, since
 * the request token would cross the network in the clear.
 *
 * No error names the request URL, the request token or any text of GitHub's answer: a refusal is
 * its status alone, a request that got no answer its error's class (`errorClass`), and no error
 * keeps another as its cause. `describe` names the request URL and the request token by their
 * variables alone, and the audience as written only where it is a plain URL (`isCleanUrl`).
 */
export function githubActionsIdentity(options: GitHubActionsIdentityOptions): TokenSource {
  // Held, so every line this process prints is scrubbed of them: a server at the wrong URL can
  // echo what it was sent (`@legion/contracts/held-secrets`).
  holdSecret(options.requestToken);
  holdUrlUserinfo(options.requestUrl);
  const send = options.fetch ?? fetch;
  return {
    async read(signal?: AbortSignal): Promise<SubjectToken> {
      const endpoint = requestEndpoint(options.requestUrl, options.audience);
      const signals = [AbortSignal.timeout(TIMEOUT_MS), signal].filter((one) => one !== undefined);
      let answer: Response;
      try {
        answer = await send(endpoint, {
          method: "GET",
          headers: { Authorization: `bearer ${options.requestToken}` },
          signal: AbortSignal.any(signals),
          redirect: "manual",
        });
      } catch (error) {
        throw new Error(`${ASKING}: no answer: ${errorClass(error)}`);
      }
      if (answer.status < 200 || answer.status > 299) {
        await answer.body?.cancel().catch(() => undefined);
        throw new Error(`${ASKING}: HTTP ${answer.status}`);
      }
      let text: string;
      try {
        text = await answerText(answer);
      } catch (error) {
        throw new Error(`${ASKING}: no answer: ${errorClass(error)}`);
      }
      let parsed: unknown;
      try {
        parsed = JSON.parse(text);
      } catch {
        throw new Error(`${ASKING}: HTTP ${answer.status}: the answer is not JSON`);
      }
      const value =
        typeof parsed === "object" && parsed !== null && "value" in parsed
          ? parsed.value
          : undefined;
      if (typeof value !== "string" || value === "") {
        throw new Error(`${ASKING}: the answer holds no value`);
      }
      return { token: value, tokenType: SUBJECT_TOKEN_TYPE_GITHUB };
    },
    describe: () => {
      const audience = isCleanUrl(options.audience)
        ? `${options.audience} (${MACHINE_AUDIENCE_VAR})`
        : `the audience ${MACHINE_AUDIENCE_VAR} names`;
      return `this GitHub Actions run's OIDC token for ${audience}, asked for at the URL ${ACTIONS_ID_TOKEN_REQUEST_URL_VAR} names with the token ${ACTIONS_ID_TOKEN_REQUEST_TOKEN_VAR} names`;
    },
  };
}
