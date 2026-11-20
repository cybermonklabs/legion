// packages/broker-client/src/__tests__/device-login.test.ts
//
// A machine with no platform identity of its own, such as a person's laptop, logs in through the
// broker's device grant (RFC 8628) and trades the access token that login holds
// (`AGENT_SECRETS_MACHINE_IDENTITY=device-login`). These rows hold each step to what the broker
// checks: the request object `machine.Service.verifyPersonLogin` verifies, the poll and every later
// request proved with the login's own key, the login kept in a 0600 file, a refresh under the
// login's lock that keeps the rotated refresh token, and the trade of the login's access token
// with no actor. The two-process row lives in envoy-client's device-login.test.ts.

import { afterEach, expect, test } from "bun:test";
import { createPrivateKey, createPublicKey } from "node:crypto";
import { readFile, stat, utimes, writeFile } from "node:fs/promises";
import { hostname } from "node:os";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { calculateJwkThumbprint, compactVerify, EmbeddedJWK, type JWK } from "jose";
import {
  STAND_IN_CREDENTIAL,
  STAND_IN_USER_CODE,
  stubDeviceLoginBroker,
  writeDeviceLoginFile,
} from "../../test/device-logins";
import {
  DEVICE_LOGIN_FILE_VAR,
  DeviceLoginIdentity,
  deviceLoginFile,
  deviceLoginHost,
  deviceLoginSessionName,
  finishDeviceLogin,
  readDeviceLogin,
  startDeviceLogin,
  withDeviceLoginLock,
} from "../device-login";
import {
  BROKER_URL_VAR,
  brokerTokenClient,
  IDENTITY_DEVICE_LOGIN,
  IDENTITY_VAR,
  machineIdentity,
  PROJECTS_VAR,
  SESSION_VAR,
} from "../environment";
import { DEVICE_LOGIN_COMMAND, isFinalRefusal } from "../token-errors";

const PERSON = "ada@example.com";
const HOST = "adas-laptop.local";

const makeDirectory = temporaryDirectories("device-login");

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A stand-in broker stopped after the test. */
function broker(options: Parameters<typeof stubDeviceLoginBroker>[0] = {}) {
  const stub = stubDeviceLoginBroker(options);
  stops.push(stub.stop);
  return stub;
}

/** A device login file's path in a directory of this test's own. */
function loginPath(): string {
  return join(makeDirectory(), "legion", "device-login.json");
}

/** A sleep that waits for nothing, so a poll's interval costs a test no time. */
const noSleep = async (): Promise<void> => {};

test("the device authorization signs the request object the broker's verifyPersonLogin verifies", async () => {
  const stub = broker();
  const started = await startDeviceLogin({ brokerUrl: `${stub.url}/`, person: PERSON, host: HOST });
  expect(started.userCode).toBe(STAND_IN_USER_CODE);
  expect(started.verificationUri).toBe(`${stub.url}/credentials/machine`);
  expect(started.verificationUriComplete).toBeUndefined();

  expect(stub.authorizations).toHaveLength(1);
  const { form, proofHeader } = stub.authorizations[0] as (typeof stub.authorizations)[number];
  // The machine-login client and its one scope, the key's thumbprint and the request object, and
  // nothing else: the device authorization carries no proof.
  expect([...form.keys()].sort()).toEqual(["client_id", "dpop_jkt", "request", "scope"]);
  expect(form.get("client_id")).toBe("agent-secrets-helper");
  expect(form.get("scope")).toBe("machine");
  expect(proofHeader).toBeNull();

  // What record.VerifyRequestObject checks: one ES256 JWS of typ agent-secrets-request+jwt, the
  // P-256 key it embeds, iss that key's thumbprint, aud the broker, exp at most 600 s past iat.
  const verified = await compactVerify(form.get("request") ?? "", EmbeddedJWK);
  expect(verified.protectedHeader.alg).toBe("ES256");
  expect(verified.protectedHeader.typ).toBe("agent-secrets-request+jwt");
  const jwk = verified.protectedHeader.jwk as JWK;
  expect([jwk.kty, jwk.crv, Object.hasOwn(jwk, "d")]).toEqual(["EC", "P-256", false]);
  const thumbprint = await calculateJwkThumbprint(jwk);
  // verifyPersonLogin: dpop_jkt is the request object's key thumbprint.
  expect(form.get("dpop_jkt")).toBe(thumbprint);
  const claims = JSON.parse(new TextDecoder().decode(verified.payload)) as Record<string, unknown>;
  expect(Object.keys(claims).sort()).toEqual(
    ["aud", "authorization_details", "exp", "iat", "iss", "jti", "login_hint"].sort()
  );
  expect(claims.iss).toBe(thumbprint);
  // The audience is the broker as AGENT_SECRETS_URL names it, less its trailing slash.
  expect(claims.aud).toBe(stub.url);
  expect(claims.login_hint).toBe(PERSON);
  expect(claims.authorization_details).toEqual([{ type: "launcher_credential", identifier: HOST }]);
  expect(typeof claims.jti === "string" && claims.jti.length > 0).toBe(true);
  expect((claims.exp as number) - (claims.iat as number)).toBe(600);
  expect(Math.abs((claims.iat as number) - Date.now() / 1000)).toBeLessThan(60);
});

test("the poll proves the login's key, waits out a pending code and keeps the login in a 0600 file", async () => {
  const stub = broker({ polls: ["pending", "slow_down", "approve"] });
  const path = loginPath();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const waits: number[] = [];
  const login = await finishDeviceLogin(started, {
    path,
    sleep: async (ms) => {
      waits.push(ms);
    },
  });
  // One wait of the interval before each poll, and five seconds more after slow_down (RFC 8628
  // Section 3.5).
  expect(waits).toEqual([1000, 1000, 6000]);
  expect(stub.polls).toHaveLength(3);
  const thumbprint = stub.authorizations[0]?.form.get("dpop_jkt");
  for (const poll of stub.polls) {
    expect(Object.fromEntries(poll.form)).toEqual({
      grant_type: "urn:ietf:params:oauth:grant-type:device_code",
      device_code: poll.form.get("device_code") ?? "",
      client_id: "agent-secrets-helper",
    });
    expect(poll.proofThumbprint).toBe(thumbprint as string);
    expect(poll.proofClaims?.htm).toBe("POST");
    expect(poll.proofClaims?.htu).toBe(`${stub.url}/v1/oauth/token`);
  }
  expect(login.credentialId).toBe(STAND_IN_CREDENTIAL);

  expect((await stat(path)).mode & 0o777).toBe(0o600);
  expect((await stat(join(path, ".."))).mode & 0o777).toBe(0o700);
  const file = JSON.parse(await readFile(path, "utf8")) as Record<string, unknown>;
  expect(Object.keys(file).sort()).toEqual(
    [
      "access_expires_at",
      "access_token",
      "broker",
      "credential_id",
      "expires_at",
      "key",
      "refresh_token",
    ].sort()
  );
  expect(file.broker).toBe(stub.url);
  expect(file.credential_id).toBe(STAND_IN_CREDENTIAL);
  expect(file.access_token).toBe(stub.loginTokens[0] as string);
  // The key is the PKCS#8 DER, base64, of the key whose thumbprint the login is bound to.
  const key = createPrivateKey({
    key: Buffer.from(file.key as string, "base64"),
    format: "der",
    type: "pkcs8",
  });
  expect(await calculateJwkThumbprint(createPublicKey(key).export({ format: "jwk" }) as JWK)).toBe(
    thumbprint as string
  );
  expect(Number.isNaN(Date.parse(file.expires_at as string))).toBe(false);
});

test("a login nobody approves is refused, and writes no file", async () => {
  for (const answer of ["deny", "expired"] as const) {
    const stub = broker({ polls: [answer] });
    const path = loginPath();
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const refusal = await finishDeviceLogin(started, { path, sleep: noSleep }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refusal?.message).toContain(answer === "deny" ? "denied" : "expired");
    expect(refusal?.message).toContain(DEVICE_LOGIN_COMMAND);
    expect(await stat(path).catch(() => null)).toBeNull();
  }
});

test("the identity trades the login's access token with no actor, proved with the login's key", async () => {
  const stub = broker();
  const path = loginPath();
  const cached = fixtureSecret("access");
  const written = await writeDeviceLoginFile(path, stub.url, {
    access_token: cached,
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  });
  const made = await brokerTokenClient(
    {
      [BROKER_URL_VAR]: stub.url,
      [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
      [DEVICE_LOGIN_FILE_VAR]: path,
      [PROJECTS_VAR]: "OEDGE",
    },
    "ses-laptop-1"
  );
  if (made === undefined) throw new Error("the factory made no client for a device login");
  expect(made.signIn).toBeUndefined();
  const token = await made.client.token();
  made.client.close();
  expect(token.accessToken).toBe(stub.tradeTokens[0] as string);
  // A live access token in the file is spent as it is: no refresh.
  expect(stub.refreshes).toHaveLength(0);
  expect(stub.trades).toHaveLength(1);
  const trade = stub.trades[0] as (typeof stub.trades)[number];
  expect(Object.fromEntries(trade.form)).toEqual({
    grant_type: "urn:ietf:params:oauth:grant-type:token-exchange",
    client_id: "dispatch-token-exchange",
    subject_token: cached,
    subject_token_type: "urn:ietf:params:oauth:token-type:access_token",
    session: "ses-laptop-1",
    projects: "OEDGE",
  });
  // The library binds a trade of a machine login's token to that token's key.
  expect(trade.proofThumbprint).toBe(written.thumbprint);
  expect(await calculateJwkThumbprint(made.client.key.publicJwk)).toBe(written.thumbprint);
});

test("a login with no live access token is refreshed first, under its key, keeping the rotated refresh token", async () => {
  const stub = broker();
  const path = loginPath();
  const written = await writeDeviceLoginFile(path, stub.url);
  const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
  const spent: string[] = [];
  for (let trade = 0; trade < 2; trade += 1) {
    await identity.spend(async (subject) => {
      spent.push(subject.token);
      expect(subject.tokenType).toBe("urn:ietf:params:oauth:token-type:access_token");
    });
  }
  // One refresh: the access token it answered is kept in the file and spent by the second trade.
  expect(stub.refreshes).toHaveLength(1);
  const refresh = stub.refreshes[0] as (typeof stub.refreshes)[number];
  expect(Object.fromEntries(refresh.form)).toEqual({
    grant_type: "refresh_token",
    refresh_token: written.refreshToken,
    client_id: "agent-secrets-helper",
  });
  expect(refresh.proofThumbprint).toBe(written.thumbprint);
  expect(spent).toEqual([stub.loginTokens[0] as string, stub.loginTokens[0] as string]);
  const kept = await readDeviceLogin(path);
  expect(kept.refreshToken).not.toBe(written.refreshToken);
  expect(kept.accessToken).toBe(stub.loginTokens[0] as string);
  expect((await stat(path)).mode & 0o777).toBe(0o600);
});

test("a login the broker ended is a final refusal that names dispatch login --device", async () => {
  const stub = broker({ refuseRefresh: true });
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  const made = await brokerTokenClient(
    {
      [BROKER_URL_VAR]: stub.url,
      [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
      [DEVICE_LOGIN_FILE_VAR]: path,
    },
    "ses-laptop-1"
  );
  const refusal = await made?.client.token().then(
    () => null,
    (error: unknown) => error
  );
  made?.client.close();
  expect(isFinalRefusal(refusal)).toBe(true);
  expect((refusal as Error).message).toContain(DEVICE_LOGIN_COMMAND);
  expect(stub.trades).toHaveLength(0);
});

test("a login file others may read, or one made at another broker, is refused", async () => {
  const stub = broker();
  const open = loginPath();
  await writeDeviceLoginFile(open, stub.url, {}, 0o644);
  const opened = await readDeviceLogin(open).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(opened?.message).toContain("chmod 600");
  expect(opened?.message).not.toContain(open);

  const elsewhere = loginPath();
  await writeDeviceLoginFile(elsewhere, "http://127.0.0.1:1");
  const identity = new DeviceLoginIdentity({ path: elsewhere, brokerUrl: stub.url });
  const refusal = await identity
    .spend(async () => {})
    .then(
      () => null,
      (error: unknown) => error as Error
    );
  expect(refusal?.message).toContain(BROKER_URL_VAR);
  expect(refusal?.message).toContain(DEVICE_LOGIN_COMMAND);
  expect(stub.refreshes).toHaveLength(0);
});

test("the login's lock waits for a live holder and takes a stale one", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;

  // A live holder: the work runs only once the holder lets go, which it does at the third wait.
  await writeFile(lock, "another process");
  let waits = 0;
  let ranAfter = -1;
  await withDeviceLoginLock(
    path,
    async () => {
      ranAfter = waits;
    },
    {
      sleep: async () => {
        waits += 1;
        if (waits === 3) await Bun.file(lock).delete();
      },
    }
  );
  expect(ranAfter).toBe(3);
  expect(await stat(lock).catch(() => null)).toBeNull();

  // A holder gone for more than 30 s: its lock is taken with no wait at all.
  await writeFile(lock, "a process that died holding it");
  const old = new Date(Date.now() - 31_000);
  await utimes(lock, old, old);
  let staleWaits = 0;
  await withDeviceLoginLock(path, async () => {}, {
    sleep: async () => {
      staleWaits += 1;
    },
  });
  expect(staleWaits).toBe(0);

  // A caller whose signal ends stops waiting, and never runs the work.
  await writeFile(lock, "another process");
  const caller = new AbortController();
  let ran = false;
  const stopped = await withDeviceLoginLock(
    path,
    async () => {
      ran = true;
    },
    {
      signal: caller.signal,
      sleep: async () => {
        caller.abort(new Error("the caller's deadline"));
      },
    }
  ).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(stopped?.message).toBe("the caller's deadline");
  expect(ran).toBe(false);
  // The holder's lock is the holder's: a caller that gave up leaves it where it was.
  expect(await readFile(lock, "utf8")).toBe("another process");
  await Bun.file(lock).delete();
});

test("device-login is an identity of its own, named with its variables", () => {
  expect(() => machineIdentity({ [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN })).toThrow(
    `${BROKER_URL_VAR} is required by ${IDENTITY_VAR}=device-login`
  );
  const named = machineIdentity({
    [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
    [BROKER_URL_VAR]: "http://broker.test",
    HOME: "/home/ada",
  });
  expect(named?.describe()).toContain(DEVICE_LOGIN_FILE_VAR);
  // The file is ~/.config/legion/device-login.json unless the variable names another.
  expect(deviceLoginFile({ HOME: "/home/ada" })).toBe("/home/ada/.config/legion/device-login.json");
  expect(deviceLoginFile({ HOME: "/home/ada", [DEVICE_LOGIN_FILE_VAR]: "/x/login.json" })).toBe(
    "/x/login.json"
  );
});

test("a caller acting as no session trades under the session variable, else the login's own name", async () => {
  const stub = broker();
  const path = loginPath();
  const written = await writeDeviceLoginFile(path, stub.url, {
    access_token: fixtureSecret("access"),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  });
  const env = {
    [BROKER_URL_VAR]: stub.url,
    [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
    [DEVICE_LOGIN_FILE_VAR]: path,
  };
  for (const [extra, session] of [
    [{ [SESSION_VAR]: "example-terminal" }, "example-terminal"],
    [{}, deviceLoginSessionName(written.thumbprint)],
  ] as const) {
    const made = await brokerTokenClient({ ...env, ...extra });
    await made?.client.token();
    made?.client.close();
    expect(stub.trades.at(-1)?.form.get("session")).toBe(session);
  }
  expect(deviceLoginSessionName(written.thumbprint)).toBe(
    `device-login-${written.thumbprint.slice(0, 12)}`
  );
});

test("the host a login names is this machine's name in the broker's hostname form", () => {
  expect(deviceLoginHost("Adas-MacBook-Pro.local")).toBe("adas-macbook-pro.local");
  expect(deviceLoginHost("ada_laptop")).toBe("ada-laptop");
  expect(deviceLoginHost("--")).toBe("device");
  expect(deviceLoginHost(hostname())).toMatch(
    /^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*$/
  );
});
