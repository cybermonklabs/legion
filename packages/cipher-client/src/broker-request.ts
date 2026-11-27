// packages/cipher-client/src/broker-request.ts
//
// How this package sends a request `openid-client` asks for and words its failure: the request
// itself (`requestInit`), sent with a DPoP proof that carries the nonce the server last sent
// (`sendWithProof`), the HTTP status behind a failure (`statusOf`), and the one wording of a failed
// request to the broker (`brokerRefusal`). The broker's exchange (`token-client.ts`), a device
// login's requests (`device-login.ts`) and the person's identity source (`person-identity.ts`)
// send through them.

import { errorClass, urlForText } from "@legion/contracts/log-text";
import {
  type CustomFetchOptions,
  ResponseBodyError,
  WWWAuthenticateChallengeError,
} from "openid-client";
import type { DpopKey } from "./dpop";
import type { RefusalContext } from "./refusal-text";
import { BrokerRefusedError, BrokerTradeFailedError } from "./token-errors";
import { HEADER_DPOP_HOLDER } from "./wire";

/** What names the broker's URL in a text where it does not parse (`urlForText`); the constructor
 *  of `BrokerTokenClient` (`token-client.ts`) refuses such a URL, and `brokerTokenClient` names
 *  AGENT_SECRETS_URL in its refusal. A device login names it the same way (`device-login.ts`). */
export const BROKER_URL_SETTING = "the broker URL";

/**
 * The HTTP status behind `error`, or 0 where it carries none: an OAuth error answer and a
 * `WWW-Authenticate` challenge carry the status of the answer they were read from (their `cause`
 * is the body and the challenge), and a status `openid-client` reports as an unconforming answer
 * is left in the response it hangs off the error's `cause` chain. The person's identity source
 * reads its issuer's errors with it too (`person-identity.ts`).
 */
export function statusOf(error: unknown): number {
  let cause: unknown = error;
  for (let depth = 0; depth < 4; depth += 1) {
    if (cause instanceof Response) return cause.status;
    if (cause instanceof ResponseBodyError || cause instanceof WWWAuthenticateChallengeError) {
      return cause.status;
    }
    if (typeof cause !== "object" || cause === null || !("cause" in cause)) return 0;
    cause = cause.cause;
  }
  return 0;
}

/**
 * What a failed request to the broker's `endpoint` is, the one way every client of this package
 * words it: a trade, and a device login's code request, poll and refresh. An OAuth error the
 * broker answered with a 4xx is its refusal of `refused` (`BrokerRefusedError`), which shows the
 * code only where it is one the clients know and the description only where it is a known
 * refusal; anything else (no answer, a 5xx, a 4xx with no OAuth error, an answer that is not the
 * grant's) is a failure of `doing` it (`BrokerTradeFailedError`), named by the status where there
 * is one and by the error's class, never by its message, which can repeat the URL it was handed,
 * and with no `cause`: an answer the library refused hangs off its error (oauth4webapi's `cause`
 * is the body it read), and that answer can hold a minted token, which no log may carry.
 * `context` is what the request sent that a known refusal may name.
 */
export function brokerRefusal(
  doing: string,
  refused: string,
  endpoint: string,
  error: unknown,
  context: RefusalContext = {}
): BrokerRefusedError | BrokerTradeFailedError {
  if (error instanceof ResponseBodyError && error.status >= 400 && error.status < 500) {
    // The library checks only that `error` is a string; a description of any other type is a
    // peer's value the client reads as none.
    const description: unknown = error.error_description;
    return new BrokerRefusedError(
      error.status,
      error.error,
      typeof description === "string" ? description : "",
      refused,
      context
    );
  }
  const status = statusOf(error);
  const said = status === 0 ? errorClass(error) : `HTTP ${status}: ${errorClass(error)}`;
  return new BrokerTradeFailedError(
    `${doing} ${refused} at ${urlForText(endpoint, BROKER_URL_SETTING)}: ${said}`
  );
}

/**
 * The request `openid-client` asked for in `options`, sent with `headers`: its method, its body,
 * its redirect rule, and its own signal beside the caller's `signal`, either of which aborts it.
 * The broker's exchange and the person's identity source (`person-identity.ts`) both send what
 * the library asks through it.
 */
export function requestInit(
  options: CustomFetchOptions,
  signal: AbortSignal | undefined,
  headers: Headers | Record<string, string>
): RequestInit {
  const signals = [options.signal, signal].filter((one) => one !== undefined);
  return {
    method: options.method,
    headers,
    // openid-client's own body union is wider than one `fetch` typing or another admits; the
    // bytes are the form it encoded, and every runtime this package runs on sends them.
    body: options.body as RequestInit["body"],
    redirect: options.redirect,
    ...(signals.length === 0 ? {} : { signal: AbortSignal.any(signals) }),
  };
}

/** The most recent `DPoP-Nonce` a server sent one client, which every later proof to it carries
 *  (RFC 9449 Section 8). */
export interface DpopNonce {
  value: string;
}

/**
 * One request `openid-client` asked for in `options`, sent through `fetch` with a DPoP proof of
 * `key` naming the request's method and URL and the nonce `nonce` holds, and sent once more when
 * the server answers `use_dpop_nonce` with a nonce to carry. A nonce any answer sends is kept in
 * `nonce` for the next proof. Where `holderKey` is given, each send also carries a proof of the
 * same request, nonce and all, signed with it ({@link HEADER_DPOP_HOLDER}). The broker's exchange
 * and a device login's poll and refresh (`device-login.ts`) send through it, so every request to
 * the token endpoint is proved alike.
 */
export async function sendWithProof(
  proved: {
    readonly key: DpopKey;
    readonly holderKey?: DpopKey | undefined;
    readonly fetch: typeof fetch;
    readonly nonce: DpopNonce;
  },
  url: string,
  options: CustomFetchOptions,
  signal: AbortSignal | undefined
): Promise<Response> {
  const send = async (): Promise<Response> => {
    const headers = new Headers(options.headers);
    const request = { method: options.method, url, nonce: proved.nonce.value };
    headers.set("DPoP", await proved.key.proof(request));
    if (proved.holderKey !== undefined) {
      headers.set(HEADER_DPOP_HOLDER, await proved.holderKey.proof(request));
    }
    return await proved.fetch(url, requestInit(options, signal, headers));
  };
  let answer = await send();
  const given = answer.headers.get("DPoP-Nonce") ?? "";
  if (given !== "") proved.nonce.value = given;
  if (answer.status === 400 && given !== "") {
    const body: unknown = await answer
      .clone()
      .json()
      .catch(() => null);
    const asked =
      typeof body === "object" &&
      body !== null &&
      "error" in body &&
      body.error === "use_dpop_nonce";
    if (asked) answer = await send();
  }
  return answer;
}
