// packages/envoy-client/src/__tests__/broker-credential-registration.test.ts
//
// The credential of a pod session the broker holds no registration for yet: a call that passes a
// deadline fails at it naming the session, a call that passes none waits, and both go through
// once the registration lands.

import { expect, spyOn, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { brokerCredential } from "../broker-credential";
import { brokerStandIns } from "./broker-stand-ins";

const { podEnvironment, servers } = brokerStandIns("broker-credential-registration");

/** The session a pod of the registration tests trades under. */
const POD_SESSION = "implementer-g1";

/** A broker that refuses every trade as unregistered (`registeredPodSession`) until `registered()`
 *  says the pod's deployment registered its session, and then mints a random token, kept in
 *  `minted`. */
function registeringBroker(registered: () => boolean): {
  url: string;
  trades: number[];
  minted: string[];
} {
  const trades: number[] = [];
  const minted: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      await request.text();
      trades.push(trades.length + 1);
      if (!registered()) {
        return Response.json(
          {
            error: "invalid_grant",
            error_description: `pod_session_not_registered: No registration says this pod may write under the session name '${POD_SESSION}'. A pod writes under a session name its deployment registered for it.`,
          },
          { status: 400 }
        );
      }
      const token = fixtureSecret("broker");
      minted.push(token);
      return Response.json({
        access_token: token,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, trades, minted };
}

/** Turns of the event loop until `ready()` holds: no timer, so the test runs at the client's pace. */
async function until(ready: () => boolean): Promise<void> {
  while (!ready()) await new Promise((resolve) => setImmediate(resolve));
}

/** What `call` settled with by the next turn of the event loop, or "pending": the test's bound. A
 *  credential answers headers or a promise of them, so either is taken. */
async function outcome(call: unknown): Promise<unknown> {
  const bound = Promise.withResolvers<"pending">();
  setImmediate(() => bound.resolve("pending"));
  return await Promise.race([
    Promise.resolve(call).then(
      (value) => value,
      (error: unknown) => error
    ),
    bound.promise,
  ]);
}

test("an unregistered pod session's call fails at its deadline naming the session, and the next call goes through once the registration lands", async () => {
  let registered = false;
  const broker = registeringBroker(() => registered);
  const warned = spyOn(console, "warn").mockImplementation(() => {});
  try {
    const credential = brokerCredential(
      podEnvironment(broker.url, "http://dispatch.test"),
      POD_SESSION
    );
    const url = "http://dispatch.test/v1/issues";
    // The call's deadline is a signal the test ends, standing in for the call's own timer.
    const deadline = new AbortController();
    const call = credential.headers("GET", url, undefined, deadline.signal);
    await until(() => warned.mock.calls.length === 1);
    expect(String(warned.mock.calls[0]?.[0])).toContain(POD_SESSION);
    deadline.abort();
    const refused = await outcome(call);
    expect(refused, "the call did not return at its deadline").toBeInstanceOf(Error);
    expect((refused as Error).name).toBe("RegistrationPendingError");
    expect((refused as Error).message).toContain(POD_SESSION);
    expect((refused as Error).message).toContain("retried");
    // The control: a call that passes no deadline does not return while the registration is
    // missing.
    const waiting = credential.headers("GET", url);
    expect(await outcome(waiting)).toBe("pending");
    registered = true;
    const next = await credential.headers("GET", url, undefined, new AbortController().signal);
    expect(broker.minted.map((token) => `DPoP ${token}`)).toContain(next.Authorization);
    expect(((await waiting) as Record<string, string>).Authorization).toBe(next.Authorization);
  } finally {
    warned.mockRestore();
  }
});
