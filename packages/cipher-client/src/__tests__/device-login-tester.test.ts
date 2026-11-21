// packages/cipher-client/src/__tests__/device-login-tester.test.ts
//
// Two defects the device login's test round found on the real broker (cmd/cipherd through
// scripts/dev-broker.sh), each pinned here as the person meets it: what the trade's refusal says.
//
// 1. A trade waits for the login's lock inside its own attempt (`TRADE_ATTEMPT_MS`, 30 s), and a
//    wait that outlasts the attempt is reported as "the secrets broker did not answer the trade",
//    while the broker was never asked: another process of this machine held the lock. On the dev
//    stack a session waiting on a holder that stayed alive (it ran as a background job of a
//    non-interactive shell, which ignores SIGINT, so the SIGINT meant to end it did not) failed
//    `dispatch whoami` with exit 1 and that text, and the broker minted nothing for its session.
// 2. Once a login's launcher credential is revoked, the broker answers the trade of the access
//    token the file still keeps `400 invalid_request` ("Token is not valid or has expired."), and
//    the client prints that refusal with its text withheld and no cure, until the kept token is
//    within a minute of its expiry (up to 14 minutes with the broker's 900 s tokens). Only a
//    refresh would show the login ended and name `dispatch login --device`.

import { afterEach, expect, test } from "bun:test";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { stubDeviceLoginBroker, writeDeviceLoginFile } from "../../test/device-logins";
import { DeviceLoginIdentity, withDeviceLoginLock } from "../device-login";
import { DEVICE_LOGIN_FILE_VAR } from "../device-login-file";
import {
  BROKER_URL_VAR,
  brokerTokenClient,
  IDENTITY_DEVICE_LOGIN,
  IDENTITY_VAR,
} from "../environment";
import { BrokerTokenClient } from "../token-client";
import { DEVICE_LOGIN_COMMAND } from "../token-errors";

const makeDirectory = temporaryDirectories("device-login-tester");

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

test("a trade given up while another live holder keeps the login's lock names the lock, not the broker", async () => {
  const stub = stubDeviceLoginBroker();
  stops.push(stub.stop);
  const path = join(makeDirectory(), "legion", "device-login.json");
  await writeDeviceLoginFile(path, stub.url, {
    access_token: fixtureSecret("access"),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  });
  // Another session's trade holds the lock until this one's 300 ms attempt has ended. The lock
  // file cannot tell a holder in this process from one in another: each writes a holder value of
  // its own. The attempt bounds the whole trade, the lock's wait with it, so the trade is given up.
  const holding = Promise.withResolvers<void>();
  const release = Promise.withResolvers<void>();
  const holder = withDeviceLoginLock(path, async () => {
    holding.resolve();
    await release.promise;
  });
  await holding.promise;
  const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
  const client = new BrokerTokenClient({
    brokerUrl: stub.url,
    key: await identity.key(),
    subject: identity,
    session: "ses-waiting",
    attemptMs: 300,
    log: () => {},
  });
  const refusal = await client.token().then(
    () => null,
    (error: unknown) => error as Error
  );
  client.close();
  release.resolve();
  await holder;
  // The broker was never asked: the wait was this machine's own lock.
  expect(stub.trades).toHaveLength(0);
  expect(stub.refreshes).toHaveLength(0);
  expect(refusal).not.toBeNull();
  expect(refusal?.message).not.toContain("did not answer");
  expect(refusal?.message).toContain("lock");
});

test("a trade of a revoked login's kept access token names dispatch login --device", async () => {
  // The real broker's answers to a login whose launcher credential was revoked, as the dev stack
  // gave them: the exchange of the access token the file keeps, and the refresh.
  const asked: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const form = new URLSearchParams(await request.text());
      const grant = form.get("grant_type") ?? "";
      asked.push(grant);
      if (grant === "refresh_token") {
        return Response.json(
          {
            error: "invalid_grant",
            error_description:
              "The provided authorization grant (e.g., authorization code, resource owner credentials) or refresh token is invalid, expired, revoked, does not match the redirection URI used in the authorization request, or was issued to another client.",
          },
          { status: 400 }
        );
      }
      return Response.json(
        {
          error: "invalid_request",
          error_description:
            "The request is missing a required parameter, includes an invalid parameter value, includes a parameter more than once, or is otherwise malformed. Token is not valid or has expired.",
        },
        { status: 400 }
      );
    },
  });
  stops.push(() => server.stop(true));
  const url = `http://127.0.0.1:${server.port}`;
  const path = join(makeDirectory(), "legion", "device-login.json");
  await writeDeviceLoginFile(path, url, {
    access_token: fixtureSecret("access"),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  });
  const made = await brokerTokenClient(
    {
      [BROKER_URL_VAR]: url,
      [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
      [DEVICE_LOGIN_FILE_VAR]: path,
    },
    "ses-laptop-1"
  );
  if (made === undefined) throw new Error("the factory made no client for a device login");
  const refusal = await made.client.token().then(
    () => null,
    (error: unknown) => error as Error
  );
  made.client.close();
  expect(asked).toContain("urn:ietf:params:oauth:grant-type:token-exchange");
  expect(refusal?.message).toContain(DEVICE_LOGIN_COMMAND);
});
