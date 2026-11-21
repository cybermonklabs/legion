// `keyedLayer` puts a test's stand-in of Dispatch behind Dispatch's keyed layer, so each answer the
// layer judged (each 2xx, unless the test says more) names the keyed write's key back, as Dispatch's
// own answers do. Every stand-in of a keyed write in this repository's tests answers through it, but
// two kinds, each of which says so: a stand-in on a raw socket, which no fetch-shaped layer wraps,
// names the key back itself, and a row whose answer names no key or another request's key on
// purpose is about such an answer. The other packages' tests take it as
// `@legion/envoy-client/test/keyed-layer`.
import { IDEMPOTENCY_KEY_HEADER } from "@legion/contracts";

/** How a stand-in's answers pass Dispatch's keyed layer (`keyedLayer`). */
export interface KeyedLayerOptions {
  /**
   * Which of the stand-in's answers to a keyed request the layer judged, and so name the key
   * back: each 2xx where this is not given. The layer names the key back on each answer it
   * judged, a refusal or a 5xx too; a refusal a stand-in gives may be the router's no-route
   * answer or the token check's, which come before the layer and name none, so a test whose
   * refusal is the layer's own says so here.
   */
  readonly judged?: (response: Response) => boolean;
  /**
   * Which of the stand-in's answers to a keyed request name no key back: each one (`true`), or
   * each one this picks by its number (from 1, among the requests that carry a key). Dispatch's
   * keyed layer names the key back on each answer it judges, so a row about an answer that names
   * none says so here; no other stand-in leaves the key out.
   */
  readonly namesNoKey?: true | ((request: number) => boolean);
}

/**
 * `answer`, a stand-in of Dispatch (a client's `fetch`, or a server's request handler), behind
 * Dispatch's keyed layer (`keyedWrites`, `packages/envoy/internal/dispatch/api/request_keys.go`):
 * each answer the layer judged (`options.judged`, each 2xx by default) to a request that carries
 * an `Idempotency-Key` names that key back, as `TestEveryKeyedAnswerNamesItsKey` holds every
 * Dispatch answer to a keyed write to, unless the stand-in named a key itself or the row's answer
 * names none (`options.namesNoKey`), none of whose 2xx the client takes as its call's answer
 * (`DispatchClient.#writePayload`). Every other answer is as the stand-in gave it.
 */
export function keyedLayer<Args extends [Request | string | URL, ...unknown[]]>(
  answer: (...args: Args) => Response | Promise<Response>,
  options: KeyedLayerOptions = {}
): (...args: Args) => Promise<Response> {
  const judged = options.judged ?? ((response: Response) => response.ok);
  const namesNoKey = options.namesNoKey;
  let keyed = 0;
  return async (...args) => {
    const [input, init] = args;
    const sent =
      input instanceof Request
        ? input.headers
        : new Headers((init as RequestInit | undefined)?.headers);
    const key = sent.get(IDEMPOTENCY_KEY_HEADER) ?? "";
    const request = key === "" ? 0 : ++keyed;
    const response = await answer(...args);
    const unkeyed = typeof namesNoKey === "function" ? namesNoKey(request) : namesNoKey === true;
    if (
      key === "" ||
      unkeyed ||
      !judged(response) ||
      response.headers.has(IDEMPOTENCY_KEY_HEADER)
    ) {
      return response;
    }
    const headers = new Headers(response.headers);
    headers.set(IDEMPOTENCY_KEY_HEADER, key);
    return new Response(response.body, {
      status: response.status,
      statusText: response.statusText,
      headers,
    });
  };
}
