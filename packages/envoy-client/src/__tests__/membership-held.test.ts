// packages/envoy-client/src/__tests__/membership-held.test.ts
//
// What a session's broker credential keeps while Dispatch holds the person it names. A hold is
// not a revocation: it ends at the person's next Dispatch sign-in, or the next refresh that
// confirms them, and the credential the session holds is the one the broker and Dispatch serve
// again then. So the broker's `membership_held` refusal of a person's trade is not remembered as
// a final refusal of their sign-in, and Dispatch's `401 MEMBERSHIP_HELD` to a broker token marks
// nothing stale.

import { afterEach, expect, spyOn, test } from "bun:test";
import { writeFileSync } from "node:fs";
import * as path from "node:path";
import { signedIn, stubIssuer, writeSignIn } from "@legion/broker-client/test/sign-ins";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { brokerCredential, forgetBrokerClients } from "../broker-credential";
import { type DispatchEnvironment, dispatchSettings } from "../dispatch-config";
import { DispatchClient } from "../dispatch-http";
import { readEnvoyConfig } from "../envoy-config";

afterEach(forgetBrokerClients);

const servers: { stop: (closeActive?: boolean) => void }[] = [];
afterEach(() => {
  for (const server of servers.splice(0)) server.stop(true);
});

const tempDir = temporaryDirectories("membership-held");

const PERSON = "ada@example.com";
const CLIENT_ID = "example-device-client";

/** The broker's description of its `membership_held` refusal of a trade naming {@link PERSON}. */
const HELD = `${PERSON}'s credentials are held by Dispatch; they work again when ${PERSON} signs in to Dispatch, or when Dispatch can confirm ${PERSON} with Google again`;

/** A broker that refuses every trade `membership_held` while `held.now` is set, and otherwise
 *  mints a token good for ten minutes, numbering each. */
function holdingBroker(held: { now: boolean }): { url: string; trades: URLSearchParams[] } {
  const trades: URLSearchParams[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      trades.push(new URLSearchParams(await request.text()));
      if (held.now) {
        return Response.json(
          { error: "membership_held", error_description: HELD },
          { status: 400 }
        );
      }
      return Response.json({
        access_token: `broker-token-${trades.length}`,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, trades };
}

/** A Dispatch that answers every request `answer`, keeping the Authorization each carried. */
function stubDispatch(answer: () => Response): { url: string; asked: string[] } {
  const asked: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      asked.push(request.headers.get("Authorization") ?? "");
      return answer();
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, asked };
}

/** The environment of a pod that trades its projected token at `brokerUrl`, with no Dispatch
 *  bearer at all. */
function podEnvironment(brokerUrl: string, dispatchUrl: string): DispatchEnvironment {
  const tokenFile = path.join(tempDir(), "token");
  writeFileSync(tokenFile, "projected.service.account.token\n");
  return {
    HOME: tempDir(),
    DISPATCH_URL: dispatchUrl,
    ENVOY_URL: dispatchUrl,
    AGENT_SECRETS_URL: brokerUrl,
    AGENT_SECRETS_MACHINE_IDENTITY: "kubernetes",
    AGENT_SECRETS_MACHINE_TOKEN_FILE: tokenFile,
  };
}

/** A Dispatch client of `env` for a caller acting as `session`. */
function dispatchOf(env: DispatchEnvironment, session: string): DispatchClient {
  const config = readEnvoyConfig(path.join(tempDir(), "envoy.json"), env, { cwd: tempDir() });
  const resolution = dispatchSettings(env, config, { session });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  return new DispatchClient(resolution.url, resolution.credential);
}

/** What `call` failed with, or null where it succeeded. */
async function failureOf(call: Promise<unknown>): Promise<unknown> {
  return await call.then(
    () => null,
    (error: unknown) => error
  );
}

test("membership_held keeps a sign-in broker credential", async () => {
  const held = { now: true };
  const broker = holdingBroker(held);
  const dispatch = stubDispatch(() => Response.json([]));
  const issuer = await stubIssuer({ clientId: CLIENT_ID, person: PERSON });
  servers.push(issuer);
  // A credential made from a sign-in record: the case broker-credential.ts remembers refusals for.
  const signins = path.join(tempDir(), "signins");
  await writeSignIn(signins, await signedIn(PERSON, { client_id: CLIENT_ID }));
  const secret = path.join(tempDir(), "client-secret");
  writeFileSync(secret, "example-device-client-secret\n", { mode: 0o600 });
  const env: DispatchEnvironment = {
    ...podEnvironment(broker.url, dispatch.url),
    AGENT_SECRETS_SIGNIN_DIR: signins,
    AGENT_SECRETS_SIGNIN_ISSUER: issuer.url,
    AGENT_SECRETS_SIGNIN_CLIENT_ID: CLIENT_ID,
    AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE: secret,
  };

  const during = await failureOf(dispatchOf(env, "ses_01a9").listAllIssues({}));
  expect(String(during)).toContain(`HTTP 400 membership_held: ${HELD}`);
  expect(dispatch.asked).toHaveLength(0);

  held.now = false;
  const after = await failureOf(dispatchOf(env, "ses_01a9").listAllIssues({}));
  expect(
    after,
    "after the release the call failed with no new trade: broker-credential.ts remembered membership_held as a final refusal of the sign-in (isFinalRefusal)"
  ).toBeNull();
  // The person's trade, refused while held, then traded again under the same sign-in.
  expect(broker.trades.map((form) => form.get("subject_token_type"))).toEqual([
    "urn:envoy:params:oauth:token-type:person-google-identity",
    "urn:envoy:params:oauth:token-type:person-google-identity",
  ]);
  expect(dispatch.asked).toEqual(["DPoP broker-token-2"]);
});

test("membership_held marks no credential stale", async () => {
  const broker = holdingBroker({ now: false });
  const dispatch = stubDispatch(() =>
    Response.json(
      {
        code: "MEMBERSHIP_HELD",
        error: HELD,
      },
      { status: 401 }
    )
  );
  const credential = brokerCredential(podEnvironment(broker.url, dispatch.url), "session-one");
  const stale = spyOn(credential, "stale");
  const client = new DispatchClient(dispatch.url, credential);

  for (let call = 0; call < 2; call++) {
    await expect(client.listAllIssues({})).rejects.toMatchObject({
      status: 401,
      code: "MEMBERSHIP_HELD",
    });
  }
  expect(stale, "401 MEMBERSHIP_HELD marked the broker credential stale").not.toHaveBeenCalled();
  // Each call went once, presenting the token in hand: nothing dropped it, and nothing traded
  // again at the broker.
  expect(dispatch.asked).toEqual(["DPoP broker-token-1", "DPoP broker-token-1"]);
  expect(broker.trades).toHaveLength(1);
});
