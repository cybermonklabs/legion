// packages/cipher-client/src/__tests__/device-login.test.ts
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
import { chmod, mkdir, readFile, stat, utimes, writeFile } from "node:fs/promises";
import { hostname } from "node:os";
import { dirname, join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { calculateJwkThumbprint, compactVerify, EmbeddedJWK, type JWK } from "jose";
import {
  STAND_IN_CREDENTIAL,
  STAND_IN_UNKNOWN_CODE,
  STAND_IN_USER_CODE,
  stubDeviceLoginBroker,
  writeDeviceLoginFile,
} from "../../test/device-logins";
import { sentAs } from "../../test/refusal-corpus";
import {
  DEVICE_LOGIN_FILE_VAR,
  DeviceLoginIdentity,
  deviceLoginHost,
  finishDeviceLogin,
  readDeviceLogin,
  refreshDeviceLogin,
  startDeviceLogin,
  withDeviceLoginLock,
} from "../device-login";
import { DEVICE_LOGIN_STALE_LOCK_MS } from "../device-login-lock";
import {
  BROKER_URL_VAR,
  brokerTokenClient,
  IDENTITY_DEVICE_LOGIN,
  IDENTITY_VAR,
  PROJECTS_VAR,
} from "../environment";
import { TRADE_ATTEMPT_MS } from "../token-client";
import {
  BrokerRefusedError,
  BrokerTradeFailedError,
  DEVICE_LOGIN_COMMAND,
  DeviceLoginEndedError,
  isFinalRefusal,
} from "../token-errors";
import { POD_SESSION_NOT_REGISTERED } from "../wire";

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

test("a poll the broker refuses with an error it does not expect stops at once, naming it", async () => {
  const stub = broker({ polls: ["invalid_client"] });
  const path = loginPath();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const refusal = await finishDeviceLogin(started, { path, sleep: noSleep }).then(
    () => null,
    (error: unknown) => error as Error
  );
  // One poll, not one every interval until the code lapses and a refusal that says it expired.
  expect(stub.polls).toHaveLength(1);
  expect(refusal?.message).toContain("invalid_client");
  expect(refusal?.message).not.toContain("expired");
  expect(refusal?.message).toContain(DEVICE_LOGIN_COMMAND);
  expect(await stat(path).catch(() => null)).toBeNull();
});

test("a poll the broker answers with a 4xx that carries no OAuth error stops at once, naming the status", async () => {
  // An HTML page, as a proxy or a load balancer in front of the broker answers, and a bare DPoP
  // challenge: neither is an answer a later poll changes.
  for (const [answer, status] of [
    ["html-403", 403],
    ["challenge-401", 401],
  ] as const) {
    const stub = broker({ polls: [answer] });
    const path = loginPath();
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const refusal = await finishDeviceLogin(started, { path, sleep: noSleep }).then(
      () => null,
      (error: unknown) => error as Error
    );
    // One poll, not one every interval until the code lapses and a refusal that says it expired.
    expect(stub.polls, answer).toHaveLength(1);
    expect(refusal?.message, answer).toContain(`HTTP ${status}`);
    expect(refusal?.message, answer).not.toContain("expired");
    expect(refusal?.message, answer).toContain(DEVICE_LOGIN_COMMAND);
    expect(await stat(path).catch(() => null)).toBeNull();
  }
});

test("a poll the broker answers with its 429 or a 5xx is polled again", async () => {
  const stub = broker({ polls: ["busy-429", "unavailable-503", "approve"] });
  const path = loginPath();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const login = await finishDeviceLogin(started, { path, sleep: noSleep });
  expect(stub.polls).toHaveLength(3);
  expect(login.credentialId).toBe(STAND_IN_CREDENTIAL);
});

test("a poll the broker did not answer is polled again", async () => {
  const stub = broker();
  const path = loginPath();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  // The first poll never reaches the broker; the second does, and is approved.
  let sent = 0;
  const login = await finishDeviceLogin(started, {
    path,
    sleep: noSleep,
    fetch: (async (input: string | URL | Request, init?: RequestInit) => {
      sent += 1;
      if (sent === 1) throw new TypeError("fetch failed");
      return await fetch(input, init);
    }) as typeof fetch,
  });
  expect(sent).toBe(2);
  expect(stub.polls).toHaveLength(1);
  expect(login.credentialId).toBe(STAND_IN_CREDENTIAL);
});

test("a broker refusal shows its OAuth code only where the clients know it", async () => {
  // A code is the peer's choice of characters, which can be a token the client sent it.
  const stub = broker({ polls: ["unknown-code"] });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const refusal = await finishDeviceLogin(started, { path: loginPath(), sleep: noSleep }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(stub.polls).toHaveLength(1);
  expect(refusal?.message).toContain(
    `HTTP 400 unrecognised error code (${STAND_IN_UNKNOWN_CODE.length} chars)`
  );
  expect(refusal?.message).not.toContain(STAND_IN_UNKNOWN_CODE);
  // The description of a refusal the clients do not know is withheld too.
  expect(refusal?.message).not.toContain("the stand-in answered");

  // A refusal of the device authorization itself is worded the same way.
  const refusing = broker({ refuseAuthorization: STAND_IN_UNKNOWN_CODE });
  const unstarted = await startDeviceLogin({
    brokerUrl: refusing.url,
    person: PERSON,
    host: HOST,
  }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusing.authorizations).toHaveLength(1);
  expect(unstarted?.message).toContain(
    "the secrets broker refused this machine's device login: HTTP 400 unrecognised error code"
  );
  expect(unstarted?.message).not.toContain(STAND_IN_UNKNOWN_CODE);
  expect(unstarted?.message).not.toContain("the stand-in answered");
});

test("a code is polled once a second to once a minute, every 5 s where the broker names no interval", async () => {
  for (const [interval, wait] of [
    [0.001, 1_000],
    [null, 5_000],
    // An interval past a timer's range would fire at once.
    [3_000_000, 60_000],
  ] as const) {
    const stub = broker({ interval });
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const waits: number[] = [];
    await finishDeviceLogin(started, {
      path: loginPath(),
      sleep: async (ms) => {
        waits.push(ms);
      },
    });
    expect(waits, String(interval)).toEqual([wait]);
  }
});

test("a code and an access token are taken to live an hour at most, whatever the broker says", async () => {
  const stub = broker({ expiresIn: 3_000_000, accessSeconds: 3_000_000 });
  const path = loginPath();
  const hour = 3_600_000;
  // Each lifetime is read between the call's start and its end, so no clock decides the row.
  const began = Date.now();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const startedEnded = Date.now();
  expect(started.expiresAt.getTime()).toBeGreaterThanOrEqual(began + hour);
  expect(started.expiresAt.getTime()).toBeLessThanOrEqual(startedEnded + hour);
  const polled = Date.now();
  const login = await finishDeviceLogin(started, { path, sleep: noSleep });
  const ended = Date.now();
  expect(login.accessExpiresAt?.getTime() ?? 0).toBeGreaterThanOrEqual(polled + hour);
  expect(login.accessExpiresAt?.getTime() ?? 0).toBeLessThanOrEqual(ended + hour);
});

test("a complete page the broker sends is checked as the page is", async () => {
  const sent = broker({
    verificationUriComplete: "http://127.0.0.1:1/credentials/machine?code=WDJB",
  });
  const started = await startDeviceLogin({ brokerUrl: sent.url, person: PERSON, host: HOST });
  expect(started.verificationUriComplete).toBe("http://127.0.0.1:1/credentials/machine?code=WDJB");
  for (const verificationUriComplete of [
    "javascript:alert(1)",
    "http://127.0.0.1:1/\u202e/credentials",
  ]) {
    const stub = broker({ verificationUriComplete });
    const refusal = await startDeviceLogin({
      brokerUrl: stub.url,
      person: PERSON,
      host: HOST,
    }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refusal?.message, verificationUriComplete).toContain("page");
  }
});

test("what the broker answers for a person to read is refused where it holds a control character", async () => {
  // A terminal runs an escape sequence it is sent: an OSC 52 clipboard write, an OSC 8 link.
  const clipboardWrite = "WDJB\u001b]52;c;cGFzdGVk\u0007";
  for (const [options, what] of [
    [{ userCode: clipboardWrite }, "user code"],
    // A right-to-left override, a format character, turns round what the terminal shows.
    [{ userCode: "WDJB-\u202eTHJM" }, "user code"],
    [{ verificationUri: "http://127.0.0.1:1/\u001b]8;;http://elsewhere.test\u0007" }, "page"],
    [{ verificationUri: "javascript:alert(1)" }, "page"],
  ] as const) {
    const stub = broker(options);
    const refusal = await startDeviceLogin({
      brokerUrl: stub.url,
      person: PERSON,
      host: HOST,
    }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refusal?.message, JSON.stringify(options)).toContain(what);
    expect(refusal?.message).not.toContain("\u001b");
  }
  const stub = broker({ credentialId: "cred\u001b[2Jdevice" });
  const path = loginPath();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const refusal = await finishDeviceLogin(started, { path, sleep: noSleep }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusal?.message).toContain("credential_id");
  expect(refusal?.message).not.toContain("\u001b");
  expect(await stat(path).catch(() => null)).toBeNull();
});

test("a login nobody approves is refused, and writes no file", async () => {
  // Denied, expired, and a code the broker no longer knows (`invalid_grant`), which is an expiry.
  for (const answer of ["deny", "expired", "invalid_grant"] as const) {
    const stub = broker({ polls: [answer] });
    const path = loginPath();
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const refusal = await finishDeviceLogin(started, { path, sleep: noSleep }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(stub.polls, answer).toHaveLength(1);
    expect(refusal?.message, answer).toContain(answer === "deny" ? "denied" : "expired");
    expect(refusal?.message).toContain(DEVICE_LOGIN_COMMAND);
    expect(await stat(path).catch(() => null)).toBeNull();
  }
  // A code whose lifetime has passed is not polled at all.
  const stub = broker();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const lapsed = await finishDeviceLogin(
    { ...started, expiresAt: new Date(Date.now() - 1) },
    { path: loginPath(), sleep: noSleep }
  ).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(stub.polls).toHaveLength(0);
  expect(lapsed?.message).toContain("expired");
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
      [PROJECTS_VAR]: "EXAMPLE",
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
    projects: "EXAMPLE",
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

test("a session started before another login of this machine refuses to trade the new one", async () => {
  const stub = broker();
  const path = loginPath();
  const live = {
    access_token: fixtureSecret("access"),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  };
  await writeDeviceLoginFile(path, stub.url, live);
  const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
  await identity.key();
  // `dispatch login --device` ran again since: the file holds a login of another key, which the
  // client this session made does not prove.
  await writeDeviceLoginFile(path, stub.url, live);
  const refusal = await identity
    .spend(async () => {})
    .then(
      () => null,
      (error: unknown) => error as Error
    );
  expect(refusal?.message).toContain("holds another login");
  expect(refusal?.message).toContain("start this session again");
  expect(stub.refreshes).toHaveLength(0);
});

test("a save removes the copies and locks a process that died left beside the login", async () => {
  const stub = broker();
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  const dir = dirname(path);
  const old = new Date(Date.now() - 2 * TRADE_ATTEMPT_MS);
  const leftCopy = join(dir, ".device-login-00000000-0000-4000-8000-000000000001.json.tmp");
  const leftLock = `${path}.lock.stale-00000000-0000-4000-8000-000000000002`;
  const leftSteal = `${path}.lock.steal.stale-00000000-0000-4000-8000-000000000003`;
  for (const left of [leftCopy, leftLock, leftSteal]) {
    await writeFile(left, "left by a process that died");
    await utimes(left, old, old);
  }
  // A copy written a moment ago may be a live save's, and stays.
  const freshCopy = join(dir, ".device-login-00000000-0000-4000-8000-000000000004.json.tmp");
  await writeFile(freshCopy, "a live save's");
  const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
  await identity.spend(async () => {});
  expect(stub.refreshes).toHaveLength(1);
  for (const left of [leftCopy, leftLock, leftSteal]) {
    expect(await stat(left).catch(() => null), left).toBeNull();
  }
  expect(await readFile(freshCopy, "utf8")).toBe("a live save's");
});

test("a machine with no login file is told to log in", async () => {
  const refusal = await readDeviceLogin(loginPath()).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusal?.message).toContain("this machine has no device login");
  expect(refusal?.message).toContain(`log this machine in with: ${DEVICE_LOGIN_COMMAND}`);
});

test("a login directory others may write is refused, as a file others may read is", async () => {
  const stub = broker();
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  await chmod(dirname(path), 0o775);
  const refused = await readDeviceLogin(path).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refused?.message).toContain("mode 0775");
  expect(refused?.message).toContain("chmod 700");

  // A login made into such a directory is refused before its file is written there.
  const fresh = join(makeDirectory(), "open", "device-login.json");
  await mkdir(dirname(fresh));
  await chmod(dirname(fresh), 0o777);
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const unsaved = await finishDeviceLogin(started, { path: fresh, sleep: noSleep }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(unsaved?.message).toContain("chmod 700");
  expect(await stat(fresh).catch(() => null)).toBeNull();

  // A directory others may only read and enter, as `mkdir -p` leaves the `~/.config/legion` that
  // envoy.json lives in, holds a login: its file is 0600, and no one else can replace it there.
  await chmod(dirname(path), 0o755);
  expect((await readDeviceLogin(path)).credentialId).toBe(STAND_IN_CREDENTIAL);
});

test("of two callers that find one stale lock, exactly one takes it", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  await writeFile(lock, "a process that died holding it");
  const old = new Date(Date.now() - DEVICE_LOGIN_STALE_LOCK_MS - 1_000);
  await utimes(lock, old, old);

  // B finds the lock stale and is held there, between its look and its take-over, while A finds
  // it stale too, takes it and starts its work. Then B goes on. B must find A's lock live and
  // wait for it, never take it from A: two holders would spend one refresh token twice.
  const events: string[] = [];
  let holders = 0;
  let most = 0;
  const work = (name: string, until: Promise<void>) => async () => {
    holders += 1;
    most = Math.max(most, holders);
    events.push(`${name} starts`);
    await until;
    events.push(`${name} ends`);
    holders -= 1;
  };
  const bFound = Promise.withResolvers<void>();
  const bGoes = Promise.withResolvers<void>();
  const aWorking = Promise.withResolvers<void>();
  const aEnds = Promise.withResolvers<void>();
  // B has decided once it either waits for A's lock or runs its own work.
  const bDecided = Promise.withResolvers<void>();
  const b = withDeviceLoginLock(
    path,
    async () => {
      bDecided.resolve();
      await work("B", Promise.resolve())();
    },
    {
      at: async (step) => {
        if (step !== "lock-stale") return;
        bFound.resolve();
        await bGoes.promise;
      },
      sleep: async () => {
        bDecided.resolve();
        await aEnds.promise;
      },
    }
  );
  await bFound.promise;
  const a = withDeviceLoginLock(path, async () => {
    aWorking.resolve();
    await work("A", aEnds.promise)();
  });
  await aWorking.promise;
  bGoes.resolve();
  await bDecided.promise;
  aEnds.resolve();
  await Promise.all([a, b]);
  expect(most, events.join(", ")).toBe(1);
  expect(events).toEqual(["A starts", "A ends", "B starts", "B ends"]);
  expect(await stat(lock).catch(() => null)).toBeNull();
  expect(await stat(`${lock}.steal`).catch(() => null)).toBeNull();
});

test("a holder that lost its lock while it was stopped refreshes nothing, and keeps nothing", async () => {
  // A holder stopped past the lock's stale bound, as a laptop that sleeps is, has had its lock
  // taken over: a refresh would spend the refresh token the new holder may be spending too.
  const stub = broker();
  const path = loginPath();
  const written = await writeDeviceLoginFile(path, stub.url);
  const login = await readDeviceLogin(path);
  const lost = await refreshDeviceLogin(path, login, {
    describe: "this machine's device login",
    held: async () => false,
  }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(stub.refreshes).toHaveLength(0);
  expect(lost?.message).toContain("lock beside");

  // Lost while the refresh was out: the broker's answer is not kept over what the new holder
  // keeps, so the file still holds the refresh token this holder spent.
  let reads = 0;
  const lostMeanwhile = await refreshDeviceLogin(path, login, {
    describe: "this machine's device login",
    held: async () => {
      reads += 1;
      return reads === 1;
    },
  }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(stub.refreshes).toHaveLength(1);
  expect(lostMeanwhile?.message).toContain("lock beside");
  expect((await readDeviceLogin(path)).refreshToken).toBe(written.refreshToken);
});

test(
  "a refresh the broker did not answer is sent once more at once, with the same refresh token",
  async () => {
    // The broker mints the first refresh's answer and keeps it until the second refresh arrives,
    // which is after the client gave the first up: an answer that never reached the client. The
    // client gives it up on its request's real deadline, which is the behaviour under test, so the
    // row pays that deadline once.
    const stub = broker({ refreshesBeforeAnswer: 2 });
    const path = loginPath();
    const written = await writeDeviceLoginFile(path, stub.url);
    const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
    const spent: string[] = [];
    await identity.spend(async (subject) => {
      spent.push(subject.token);
    });
    expect([...stub.refreshes.map((refresh) => refresh.form.get("refresh_token"))]).toEqual([
      written.refreshToken,
      written.refreshToken,
    ]);
    // The login kept is the second answer's, whose access token the broker honours.
    expect(spent).toEqual([stub.loginTokens[1] as string]);
    expect((await readDeviceLogin(path)).accessToken).toBe(stub.loginTokens[1] as string);
  },
  2 * TRADE_ATTEMPT_MS
);

test("a trade refused for its session or its projects is not refreshed and traded again", async () => {
  // The broker answers these with the same codes it answers a token it no longer honours with,
  // and a refresh cures none of them: it would only rotate the refresh token.
  for (const [code, description] of [
    [
      "invalid_grant",
      sentAs(
        "belongs to another identity: one the broker's configuration fixes it for, or one that has written under it within the last 30 days. Ask under a name of your own."
      ),
    ],
    [
      "invalid_request",
      sentAs(
        "is a restricted member, whose token names its projects: set AGENT_SECRETS_PROJECTS to the projects this session works on."
      ),
    ],
    // A pod session no registration names yet, which a later trade cures, not a refresh.
    ["invalid_grant", `${POD_SESSION_NOT_REGISTERED}: no registration names ses-laptop-1 yet`],
  ] as const) {
    const stub = broker();
    const path = loginPath();
    await writeDeviceLoginFile(path, stub.url, {
      access_token: fixtureSecret("access"),
      access_expires_at: new Date(Date.now() + 600_000).toISOString(),
    });
    const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
    let trades = 0;
    const refusal = await identity
      .spend(async () => {
        trades += 1;
        throw new BrokerRefusedError(400, code, description, "this machine's device login", {
          person: "ada@example.com",
          session: "ses-laptop-1",
        });
      })
      .then(
        () => null,
        (error: unknown) => error as Error
      );
    expect(trades, code).toBe(1);
    expect(stub.refreshes, code).toHaveLength(0);
    expect(refusal).toBeInstanceOf(BrokerRefusedError);
  }
});

test("a trade refused because the login ended is refreshed, and the refresh's refusal names the cure", async () => {
  const stub = broker({ refuseRefresh: true });
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url, {
    access_token: fixtureSecret("access"),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  });
  const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
  let trades = 0;
  const refusal = await identity
    .spend(async () => {
      trades += 1;
      throw new BrokerRefusedError(
        400,
        "invalid_grant",
        sentAs("The machine login the subject token belongs to has ended."),
        "this machine's device login"
      );
    })
    .then(
      () => null,
      (error: unknown) => error as Error
    );
  expect(trades).toBe(1);
  expect(stub.refreshes).toHaveLength(1);
  expect(refusal).toBeInstanceOf(DeviceLoginEndedError);
  expect(refusal?.message).toContain(DEVICE_LOGIN_COMMAND);
});

test("a refresh refused other than invalid_grant is that refusal, and one unanswered twice names the broker", async () => {
  const stub = broker({ refuseRefresh: "invalid_client" });
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  const refused = await refreshDeviceLogin(path, await readDeviceLogin(path), {
    describe: "this machine's device login",
    held: async () => true,
  }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(stub.refreshes).toHaveLength(1);
  expect(refused).toBeInstanceOf(BrokerRefusedError);
  expect(refused?.message).toContain("HTTP 401 invalid_client");

  // Nothing listens at the broker's port: the refresh is sent once more, then fails, named by
  // the broker's setting and not by its URL.
  const nowhere = loginPath();
  await writeDeviceLoginFile(nowhere, "http://127.0.0.1:1");
  const unanswered = await refreshDeviceLogin(nowhere, await readDeviceLogin(nowhere), {
    describe: "this machine's device login",
    held: async () => true,
  }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(unanswered).toBeInstanceOf(BrokerTradeFailedError);
  expect(unanswered?.message).toContain("refresh this machine's device login at");
});

test("a trade refused after its holder lost the lock is one a later trade cures, refreshed or not", async () => {
  // The holder stopped once it had read the login (and, where its token was due, refreshed and
  // kept it), and another process took the lock and refreshed meanwhile: that refresh revoked
  // this holder's access token, and the broker refuses it. The login itself is live, so the
  // refusal must not end the client: the next trade reads the login the new holder kept.
  for (const due of [false, true]) {
    const stub = broker();
    const path = loginPath();
    await writeDeviceLoginFile(
      path,
      stub.url,
      due
        ? {}
        : {
            access_token: fixtureSecret("access"),
            access_expires_at: new Date(Date.now() + 600_000).toISOString(),
          }
    );
    const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
    let trades = 0;
    const refusal = await identity
      .spend(async () => {
        trades += 1;
        await writeFile(`${path}.lock`, "another process that took the lock over");
        throw new BrokerRefusedError(
          400,
          "invalid_request",
          "Token is not valid or has expired.",
          "this machine's device login"
        );
      })
      .then(
        () => null,
        (error: unknown) => error as Error
      );
    expect(trades, `due: ${due}`).toBe(1);
    expect(stub.refreshes, `due: ${due}`).toHaveLength(due ? 1 : 0);
    expect(isFinalRefusal(refusal), `due: ${due}`).toBe(false);
    expect(refusal?.message, `due: ${due}`).toContain("lock beside");
    await Bun.file(`${path}.lock`).delete();
  }
});

test("a trade refused under a lock still held after a refresh just made is final", async () => {
  // Once with no kept token, refreshed before the first trade; once with a kept token refused,
  // refreshed, and refused again. Either way the broker refused a token it had just minted, so
  // no second refresh can cure it. The trade would go through at its next try, so a loop that
  // refreshed again would end in a token, not the refusal.
  for (const [kept, refusals] of [
    [false, 1],
    [true, 2],
  ] as const) {
    const stub = broker();
    const path = loginPath();
    await writeDeviceLoginFile(
      path,
      stub.url,
      kept
        ? {
            access_token: fixtureSecret("access"),
            access_expires_at: new Date(Date.now() + 600_000).toISOString(),
          }
        : {}
    );
    const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
    let trades = 0;
    const refusal = await identity
      .spend(async () => {
        trades += 1;
        if (trades > refusals) return "traded";
        throw new BrokerRefusedError(
          400,
          "invalid_request",
          "Token is not valid or has expired.",
          "this machine's device login"
        );
      })
      .then(
        () => null,
        (error: unknown) => error as Error
      );
    expect(trades, `kept: ${kept}`).toBe(refusals);
    expect(stub.refreshes, `kept: ${kept}`).toHaveLength(1);
    expect(refusal, `kept: ${kept}`).toBeInstanceOf(BrokerRefusedError);
    expect(isFinalRefusal(refusal), `kept: ${kept}`).toBe(true);
  }
});

test("a trade traded again after a refusal is retryable too when the lock was lost meanwhile", async () => {
  // The kept token is refused under the holder's own lock, so the holder refreshes and trades
  // again; it stopped before that second answer, and another process took the lock and
  // refreshed. The second refusal is then the new holder's doing, and a later trade cures it.
  const stub = broker();
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url, {
    access_token: fixtureSecret("access"),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  });
  const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
  let trades = 0;
  const refusal = await identity
    .spend(async () => {
      trades += 1;
      if (trades === 2) await writeFile(`${path}.lock`, "another process that took the lock over");
      throw new BrokerRefusedError(
        400,
        "invalid_request",
        "Token is not valid or has expired.",
        "this machine's device login"
      );
    })
    .then(
      () => null,
      (error: unknown) => error as Error
    );
  expect(trades).toBe(2);
  expect(stub.refreshes).toHaveLength(1);
  expect(isFinalRefusal(refusal)).toBe(false);
  expect(refusal?.message).toContain("lock beside");
  await Bun.file(`${path}.lock`).delete();
});

test("a kept access token the broker refuses is refreshed at once, and traded again", async () => {
  // Another process's refresh made the token this file keeps one the broker refuses: a refresh
  // now gets a token it honours, and the trade goes through.
  const stub = broker();
  const path = loginPath();
  const kept = fixtureSecret("access");
  await writeDeviceLoginFile(path, stub.url, {
    access_token: kept,
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  });
  const rival = await readDeviceLogin(path);
  await withDeviceLoginLock(path, (held) =>
    refreshDeviceLogin(path, rival, { describe: "the rival's login", held })
  );
  await writeDeviceLoginFile(path, stub.url, {
    refresh_token: (await readDeviceLogin(path)).refreshToken,
    access_token: kept,
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  });
  const made = await brokerTokenClient(
    {
      [BROKER_URL_VAR]: stub.url,
      [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
      [DEVICE_LOGIN_FILE_VAR]: path,
    },
    "ses-laptop-1"
  );
  const token = await made?.client.token();
  made?.client.close();
  expect(stub.revokedTrades).toHaveLength(1);
  expect(stub.refreshes).toHaveLength(2);
  expect(token?.accessToken).toBe(stub.tradeTokens[0] as string);
});

test("the host a login names is this machine's name in the broker's hostname form", () => {
  expect(deviceLoginHost("Adas-MacBook-Pro.local")).toBe("adas-macbook-pro.local");
  expect(deviceLoginHost("ada_laptop")).toBe("ada-laptop");
  expect(deviceLoginHost("--")).toBe("device");
  expect(deviceLoginHost(hostname())).toMatch(
    /^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?(\.[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?)*$/
  );
});
