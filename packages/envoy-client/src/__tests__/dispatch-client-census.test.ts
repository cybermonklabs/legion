import { expect, spyOn, test } from "bun:test";
import { IDEMPOTENCY_KEY_HEADER, IDEMPOTENCY_KEY_PATTERN } from "@legion/contracts";
import { keyedLayer } from "../../test/keyed-layer";
import { leastAnswerFor } from "../../test/route-answers";
import { bearerCredential } from "../bearer";
import { DISPATCH_WRITE_RETRY, DispatchClient } from "../dispatch-http";
import { CALLS, type ClientMethod, clientMethodNames } from "./dispatch-client-calls";

// Every write the `dispatch` command makes carries one key because every request DispatchClient
// makes goes through its one request function. This drives every method of the class through a
// recording fetch, so a method added later that reaches Dispatch any other way - its own fetch, the
// global one, a write without the key - fails here, as does a method that `CALLS`
// (dispatch-client-calls.ts) leaves out. The fetch answers a write as Dispatch does, behind its
// keyed layer: the route's least answer, naming the write's key back, which every write reads as
// its answer. `UNKEYED` names the one write sent with no key, and why.

/** The write methods whose POST or PATCH carries no `Idempotency-Key`, each with why. */
const UNKEYED: Readonly<Partial<Record<ClientMethod, string>>> = {
  sendRoute:
    "dispatch api's write of the route its caller names, sent through one #send as written, so the caller decides whether to send it again",
};

test("every DispatchClient method reaches Dispatch through its one request function, every write keyed and answered", async () => {
  // Every method of the class: a method `CALLS` leaves out, or names that the class lacks, fails.
  expect(clientMethodNames().sort()).toEqual(Object.keys(CALLS).sort());
  const global = spyOn(globalThis, "fetch");
  const keys: string[] = [];
  /** Each write call that did not end in the answer Dispatch gave it, with what it ended in. */
  const unanswered: { readonly name: string; readonly error: string }[] = [];
  try {
    for (const [name, call] of Object.entries(CALLS)) {
      const requests: { readonly method: string; readonly key: string | null }[] = [];
      const recording = keyedLayer(async (url: RequestInfo | URL, init?: RequestInit) => {
        const method = init?.method ?? "GET";
        requests.push({ method, key: new Headers(init?.headers).get(IDEMPOTENCY_KEY_HEADER) });
        const written =
          method === "POST" || method === "PATCH"
            ? leastAnswerFor(method, new URL(String(url)).pathname)
            : undefined;
        return written === undefined
          ? Response.json({})
          : Response.json(written.body, { status: written.status });
      }) as typeof fetch;
      const client = new DispatchClient(
        "http://dispatch.test",
        bearerCredential("secret"),
        recording,
        { writeRetry: DISPATCH_WRITE_RETRY }
      );

      const error = await call(client).then(
        () => undefined,
        (thrown: unknown) => (thrown instanceof Error ? thrown.message : String(thrown))
      );
      const wrote = requests.some(({ method }) => method === "POST" || method === "PATCH");
      if (wrote && error !== undefined) unanswered.push({ name, error });

      expect({ name, requested: requests.length > 0 }).toEqual({ name, requested: true });
      for (const { method, key } of requests) {
        if (
          (method === "POST" || method === "PATCH") &&
          UNKEYED[name as ClientMethod] === undefined
        ) {
          expect({ name, method, key }).toEqual({
            name,
            method,
            key: expect.stringMatching(IDEMPOTENCY_KEY_PATTERN),
          });
          keys.push(key as string);
        } else {
          expect({ name, method, key }).toEqual({ name, method, key: null });
        }
      }
    }
    expect(global).not.toHaveBeenCalled();
    expect(unanswered).toEqual([]);
  } finally {
    global.mockRestore();
  }
  // One key per write call: no two calls shared one, and the writes are many.
  expect(new Set(keys).size).toBe(keys.length);
  expect(keys.length).toBeGreaterThan(20);
});
