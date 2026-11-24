// A failing test for #4140 at fde450dadf, found by the acceptance (AcceptE2-2): a session that
// switches to a session whose pod registration is missing, then back, cannot take its own token
// again. The forward merge brought #4153's rule into the trade: with nothing live in hand it waits
// out a missing registration, trading again (15 s doubling to 5 minutes) until the registration
// lands or its caller stops waiting (`BrokerTokenClient.token`). `brokerNatsCredential`'s
// `renew` makes a caller that comes while a trade is in flight wait for that trade to settle, and
// the NATS token source passes no signal (`brokerTokenFor`). So the renewal for the session the
// host is back on waits for as long as the other session's registration is missing. On the real
// stack (the acceptance's bin/stream-scenarios.sh, failed-switch), the host's queued rebind is
// abandoned at 30 s, its reconcile pass stalls, and the first session's agent stream answers no
// replay after the return; at 20babd9838 the same run answers it. Put it in
// packages/envoy-client/src/__tests__/ and run `bun test src/__tests__/nats-switch-unregistered.test.ts`
// in packages/envoy-client.
import { afterEach, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { forgetBrokerClients } from "../broker-credential";
import type { EnvoyEnvironment } from "../envoy-settings";
import { brokerNatsCredential } from "../nats-credential";

afterEach(forgetBrokerClients);

const servers: { stop: (closeActive?: boolean) => void }[] = [];
afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

const tempDir = temporaryDirectories("nats-switch-unregistered");

/** The session whose pod registration is missing, and the one the host comes back to. */
const UNREGISTERED = "ses_b_unregistered";
const REGISTERED = "ses_a_registered";

/** A broker that refuses every trade for {@link UNREGISTERED} as a missing registration and mints
 *  a random token for any other session, recording which session each token was minted for. */
function registrationBroker() {
  const trades: string[] = [];
  const minted = new Map<string, string>();
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const session = new URLSearchParams(await request.text()).get("session") ?? "";
      trades.push(session);
      if (session === UNREGISTERED) {
        return Response.json(
          {
            error: "invalid_grant",
            error_description: `pod_session_not_registered: No registration says this pod may write under the session name '${session}'. A pod writes under a session name its deployment registered for it.`,
          },
          { status: 400 }
        );
      }
      const token = fixtureSecret("tok");
      minted.set(token, session);
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

/** The environment of a pod with an identity to trade at `brokerUrl`. */
function podEnvironment(brokerUrl: string): EnvoyEnvironment {
  const tokenFile = path.join(tempDir(), "token");
  writeFileSync(tokenFile, `${fixtureSecret("proj")}\n`);
  return {
    HOME: tempDir(),
    ENVOY_NATS_URL: "nats://nats.internal.example:4222",
    AGENT_SECRETS_URL: brokerUrl,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
  };
}

test("a session back from a session whose registration is missing takes its own token again", async () => {
  const broker = registrationBroker();
  let session = UNREGISTERED;
  const credential = brokerNatsCredential(podEnvironment(broker.url), () => session);
  // The switch: the renewal for the session whose registration is missing goes into flight.
  const away = credential.renew().then(
    () => "renewed",
    () => "refused"
  );
  while (!broker.trades.includes(UNREGISTERED)) await Bun.sleep(10);
  // The host is back on its own session, which is registered, and renews for it.
  session = REGISTERED;
  const back = await Promise.race([
    credential.renew().then(
      () => "renewed",
      (error: unknown) => `refused: ${error instanceof Error ? error.message : String(error)}`
    ),
    Bun.sleep(5_000).then(() => "still waiting after 5 s"),
  ]);
  // The row can fail for the defect: the broker was asked for the unregistered session first.
  expect(broker.trades[0]).toBe(UNREGISTERED);
  expect(back).toBe("renewed");
  expect(broker.trades).toContain(REGISTERED);
  const presented = (credential.authenticator("bm9uY2U") as { auth_token: string }).auth_token;
  expect(broker.minted.get(presented)).toBe(REGISTERED);
  void away;
}, 20_000);
