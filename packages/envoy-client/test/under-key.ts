import { IDEMPOTENCY_KEY_HEADER } from "@legion/contracts";
import { type DispatchCliIo, runDispatchCli } from "../src/dispatch-cli";
import type { ExecuteDispatchToolInput } from "../src/dispatch-commands/context";
import { type DispatchToolResult, executeDispatchTool } from "../src/dispatch-execute";

/**
 * `response` to a request of `method` that carried `key` (null for none), as Dispatch's keyed layer
 * sends it: a 2xx to a POST or PATCH names the key back (`keyedWrites`,
 * packages/envoy/internal/dispatch/api/request_keys.go). The client takes a write's 2xx without its
 * key for something in Dispatch's place (`DispatchClient.#answer`), so a fake that stands for
 * Dispatch answers under the key. A refusal or a page is left as the fake wrote it: a test that
 * models a gateway's answer, which names no key, keeps doing so.
 */
export function answerUnderKey(method: string, key: string | null, response: Response): Response {
  if (
    key === null ||
    (method !== "POST" && method !== "PATCH") ||
    !response.ok ||
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
}

/** `fetchImpl`, each answer of it as Dispatch's keyed layer sends it (`answerUnderKey`). */
export function underKey(fetchImpl: typeof fetch): typeof fetch {
  return (async (input: string | URL | Request, init?: RequestInit): Promise<Response> => {
    const response = await fetchImpl(input, init);
    const method = init?.method ?? (input instanceof Request ? input.method : "GET");
    const key =
      new Headers(init?.headers).get(IDEMPOTENCY_KEY_HEADER) ??
      (input instanceof Request ? input.headers.get(IDEMPOTENCY_KEY_HEADER) : null);
    return answerUnderKey(method, key, response);
  }) as typeof fetch;
}

/** `executeDispatchTool` against a fake that stands for Dispatch: its fetch, global fetch where it
 *  names none, answers a write under its key (`underKey`). */
export function executeDispatchToolUnderKey(
  input: ExecuteDispatchToolInput
): Promise<DispatchToolResult> {
  return executeDispatchTool({ ...input, fetchImpl: underKey(input.fetchImpl ?? fetch) });
}

/** `runDispatchCli` against a fake that stands for Dispatch: its fetch, global fetch where it names
 *  none, answers a write under its key (`underKey`). */
export function runDispatchCliUnderKey(
  argv: readonly string[],
  env: Readonly<Record<string, string | undefined>>,
  io: DispatchCliIo
): Promise<number> {
  return runDispatchCli(argv, env, { ...io, fetchImpl: underKey(io.fetchImpl ?? fetch) });
}
