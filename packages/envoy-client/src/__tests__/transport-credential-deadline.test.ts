// packages/envoy-client/src/__tests__/transport-credential-deadline.test.ts
//
// An Envoy call hands its credential the call's own deadline, so a credential that is still
// waiting (a pod whose session's registration has not landed) fails the call at that deadline
// instead of holding it. broker-credential.test.ts covers what the broker credential does with the
// deadline; this covers that the transport gives it one.

import { expect, test } from "bun:test";
import { createEnvoyClient } from "../transport";

/** The session the credential's refusal names. */
const POD_SESSION = "implementer-g1";

/** Turns of the event loop until `ready()` holds: no timer, so the test runs at the client's pace. */
async function until(ready: () => boolean): Promise<void> {
  while (!ready()) await new Promise((resolve) => setImmediate(resolve));
}

test("an Envoy call presents its own deadline to the credential, and fails with what the credential said at it", async () => {
  const fetched: string[] = [];
  const given: (AbortSignal | undefined)[] = [];
  const client = createEnvoyClient({
    baseUrl: "http://listener.test",
    retry: false,
    timeoutMs: 20,
    fetch: async (url) => {
      fetched.push(String(url));
      return Response.json({});
    },
    credential: {
      scheme: "DPoP",
      headers: (_method, _url, _session, signal) => {
        given.push(signal);
        const { promise, reject } = Promise.withResolvers<Readonly<Record<string, string>>>();
        signal?.addEventListener("abort", () => reject(new Error(`pending: ${POD_SESSION}`)), {
          once: true,
        });
        return promise;
      },
      stale: () => {},
      secret: () => "",
    },
  });
  const call = client.getRole("implementer");
  await until(() => given.length === 1);
  expect(given[0], "the call handed its credential no deadline").toBeInstanceOf(AbortSignal);
  // The transport's own deadline timer ends the signal; the transport takes no clock of a test's.
  const deadline = given[0] as AbortSignal;
  expect(deadline.aborted).toBe(false);
  await expect(call).rejects.toThrow(`pending: ${POD_SESSION}`);
  expect(deadline.aborted).toBe(true);
  // The call asked the listener nothing: past its deadline there is nothing to send.
  expect(fetched).toEqual([]);
});
