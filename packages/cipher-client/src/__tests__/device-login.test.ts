// packages/cipher-client/src/__tests__/device-login.test.ts
//
// A machine with no platform identity of its own, such as a person's laptop, logs in through the
// broker's device grant (RFC 8628) and trades the access token that login holds
// (`AGENT_SECRETS_MACHINE_IDENTITY=device-login`). These rows hold each step to what the broker
// checks: the request object `machine.Service.verifyPersonLogin` verifies, the poll and every later
// request proved with the login's own key, the login kept in a 0600 file, a refresh under the
// login's lock that keeps the rotated refresh token, and the trade of the login's access token
// with no actor. The two-process row lives in envoy-client's device-login.test.ts, the least
// life a kept access token must have left for a trade to spend it in
// device-login-least-life.test.ts, and the poll's interval and wait, and the refusal of a code no
// poll reaches, in device-login-reach.test.ts.

import { afterEach, expect, setSystemTime, spyOn, test } from "bun:test";
import { createPrivateKey, createPublicKey } from "node:crypto";
import { readFile, stat, utimes, writeFile } from "node:fs/promises";
import { hostname } from "node:os";
import { dirname, join } from "node:path";
import { BROKER_REQUEST_REASON_MAX } from "@legion/contracts";
import { holdSecret, scrub } from "@legion/contracts/held-secrets";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { calculateJwkThumbprint, compactVerify, EmbeddedJWK, type JWK } from "jose";
import {
  advancingWaits,
  recordedWaits,
  STAND_IN_ACCESS_SECONDS,
  STAND_IN_CREDENTIAL,
  STAND_IN_UNKNOWN_CODE,
  STAND_IN_USER_CODE,
  stubDeviceLoginBroker,
  writeDeviceLoginFile,
} from "../../test/device-logins";
import { sentAs } from "../../test/refusal-corpus";
import {
  DeviceLoginIdentity,
  deviceLoginHost,
  finishDeviceLogin,
  refreshDeviceLogin,
  startDeviceLogin,
  withDeviceLoginLock,
} from "../device-login";
import {
  DEVICE_LOGIN_FILE_NAMED,
  DEVICE_LOGIN_FILE_VAR,
  readDeviceLogin,
} from "../device-login-file";
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
  DeviceLoginReplacedError,
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

test("a Dispatch sign-in requests dispatch and names its sorted unique scopes", async () => {
  const stub = broker({ dispatchScopes: ["person"], scope: "dispatch" });
  const started = await startDeviceLogin({
    brokerUrl: stub.url,
    dispatchScopes: ["person", "person"],
    host: HOST,
    person: PERSON,
    reason: "Run Dispatch work",
  });

  const request = stub.authorizations[0]?.form;
  expect(request?.get("scope")).toBe("dispatch");
  const verified = await compactVerify(request?.get("request") ?? "", EmbeddedJWK);
  const claims = JSON.parse(new TextDecoder().decode(verified.payload)) as Record<string, unknown>;
  expect(claims.authorization_details).toEqual([
    { actions: ["person"], identifier: HOST, type: "launcher_credential" },
  ]);
  expect(claims.reason).toBe("Run Dispatch work");

  const path = loginPath();
  const login = await finishDeviceLogin(started, { path, sleep: noSleep });
  expect(login.dispatchScopes).toEqual(["person"]);
  expect(JSON.parse(await readFile(path, "utf8")).dispatch_scopes).toEqual(["person"]);
});

test("a Dispatch sign-in from an older broker keeps no login", async () => {
  const stub = broker({ scope: "dispatch" });
  const path = loginPath();
  const started = await startDeviceLogin({
    brokerUrl: stub.url,
    dispatchScopes: ["person"],
    host: HOST,
    person: PERSON,
  });

  const refusal = await finishDeviceLogin(started, { path, sleep: noSleep }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusal?.message).toContain("too old");
  expect(refusal?.message).toContain(`Machine logins on ${stub.url}/credentials/machine`);
  expect(await stat(path).catch(() => null)).toBeNull();
});

// The first login keeps the scopes its person ticked, some or all of those it asked for. An answer
// naming a scope it did not ask for, or one twice, is refused before anything is kept, and the
// refusal ends as the too-old one does: where to revoke the sign-in the broker may hold live, and
// the command that asks for the same sign-in again.
test("a Dispatch sign-in keeps fewer scopes than it asked for, and refuses one it did not ask for", async () => {
  const fewer = broker({ dispatchScopes: [], scope: "dispatch" });
  const started = await startDeviceLogin({
    brokerUrl: fewer.url,
    dispatchScopes: ["person"],
    host: HOST,
    person: PERSON,
  });
  const kept = await finishDeviceLogin(started, { path: loginPath(), sleep: noSleep });
  expect(kept.dispatchScopes).toEqual([]);

  for (const [name, scopes] of [
    ["a scope not asked for", ["approve"]],
    ["a scope twice", ["person", "person"]],
  ] as const) {
    const stub = broker({ dispatchScopes: scopes, scope: "dispatch" });
    const path = loginPath();
    const asking = await startDeviceLogin({
      brokerUrl: stub.url,
      dispatchScopes: ["person"],
      host: HOST,
      person: PERSON,
    });
    const refusal = await finishDeviceLogin(asking, { path, sleep: noSleep }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refusal, name).toBeInstanceOf(BrokerTradeFailedError);
    expect(refusal?.message, name).toContain(
      `revoke it in Machine logins on ${stub.url}/credentials/machine`
    );
    expect(refusal?.message, name).toEndWith(
      `then log in again with: ${DEVICE_LOGIN_COMMAND} --scope person`
    );
    expect(await stat(path).catch(() => null), name).toBeNull();
  }
});

// A broker that predates Dispatch sign-ins refuses the scope `dispatch` `400 invalid_scope` in its
// OAuth library's words, which this client does not know: the person reads the client's one
// sentence, which names both brokers that can answer so. A refusal it knows, the broker's own
// DISPATCH_SIGNIN_OFF or SCOPE_NOT_CONSENTABLE as the corpus holds them, prints in the broker's
// words. A machine login answered so, and a sign-in refused with another code, read as every
// broker refusal reads.
test("a Dispatch sign-in's request answered 400 invalid_scope in unknown words says the broker serves no sign-in", async () => {
  const sentence =
    "this broker serves no Dispatch sign-in: it predates them, or BROKER_DISPATCH_SIGNIN is off";
  const refusalOf = async (
    code: string,
    dispatchScopes: readonly string[] | undefined,
    description?: string
  ) => {
    const stub = broker({
      refuseAuthorization: code,
      ...(description === undefined ? {} : { refuseAuthorizationDescription: description }),
    });
    return await startDeviceLogin({
      brokerUrl: stub.url,
      dispatchScopes,
      host: HOST,
      person: PERSON,
    })
      .then(() => null)
      .catch((error: unknown) => error as Error);
  };
  const signIn = await refusalOf("invalid_scope", ["person"]);
  expect(signIn?.message).toBe(
    `the secrets broker refused this Dispatch sign-in: HTTP 400 invalid_scope: ${sentence}`
  );
  for (const hint of [
    "DISPATCH_SIGNIN_OFF: this broker serves no Dispatch sign-in: BROKER_DISPATCH_SIGNIN is off",
    "is not consentable; a Dispatch sign-in may approve only person, p0",
  ]) {
    const description = sentAs(hint);
    const known = await refusalOf("invalid_scope", ["person"], description);
    expect(known, hint).toBeInstanceOf(BrokerRefusedError);
    expect(known?.message, hint).toBe(
      `the secrets broker refused this Dispatch sign-in: HTTP 400 invalid_scope: ${description}`
    );
  }
  const machine = await refusalOf("invalid_scope", undefined);
  expect(machine).toBeInstanceOf(BrokerRefusedError);
  expect(machine?.message).toContain("this machine's device login");
  expect(machine?.message).not.toContain(sentence);
  const other = await refusalOf("invalid_request", ["person"]);
  expect(other).toBeInstanceOf(BrokerRefusedError);
  expect(other?.message).not.toContain(sentence);
});

// What a caller cannot ask for is refused before any request, as a RangeError a command reads as
// its usage error: a scope no Dispatch sign-in may ask for (contracts' DISPATCH_SIGN_IN_SCOPES),
// and a reason over BROKER_REQUEST_REASON_MAX characters, counted as code points as the broker
// counts them. A reason of the bound itself is sent.
test("a device login refuses a scope no sign-in may ask for and a reason past the broker's bound, before any request", async () => {
  const stub = broker();
  const wide = "\u{1F600}";
  for (const [name, more] of [
    ["approve", { dispatchScopes: ["approve"] }],
    ["an empty word", { dispatchScopes: ["person", ""] }],
    [
      "a long reason",
      { dispatchScopes: ["person"], reason: wide.repeat(BROKER_REQUEST_REASON_MAX + 1) },
    ],
  ] as const) {
    const refused = await startDeviceLogin({
      brokerUrl: stub.url,
      host: HOST,
      person: PERSON,
      ...more,
    })
      .then(() => null)
      .catch((error: unknown) => error);
    expect(refused, name).toBeInstanceOf(RangeError);
  }
  expect(stub.authorizations).toHaveLength(0);

  await startDeviceLogin({
    brokerUrl: stub.url,
    dispatchScopes: ["person"],
    host: HOST,
    person: PERSON,
    reason: wide.repeat(BROKER_REQUEST_REASON_MAX),
  });
  expect(stub.authorizations).toHaveLength(1);
});

// A Dispatch sign-in keeps its approved scopes in its file, and each refresh must answer the same
// scopes under the scope `dispatch`: an answer that drops them, narrows them, or names a machine
// login is refused and the file keeps the login it held. The refusal names the broker as
// `urlForText` does, and Dispatch's page in words, never an address built from the broker's.
test("a Dispatch sign-in's refresh that drops its scopes is refused, and the file stays", async () => {
  for (const [name, options] of [
    ["no dispatch_scopes", { scope: "dispatch" }],
    ["scope machine", { dispatchScopes: ["person"] }],
    ["fewer scopes", { scope: "dispatch", dispatchScopes: [] }],
  ] as const) {
    const stub = broker(options);
    const path = loginPath();
    await writeDeviceLoginFile(path, stub.url, { dispatch_scopes: ["person"] });
    const before = await readFile(path, "utf8");
    const refused = await refreshDeviceLogin(path, await readDeviceLogin(path), {
      describe: "this machine's device login",
      held: async () => true,
    }).then(
      () => null,
      (error: unknown) => error
    );
    expect(refused, name).toBeInstanceOf(BrokerTradeFailedError);
    expect((refused as Error).message, name).toContain(stub.url);
    expect((refused as Error).message, name).toContain("Dispatch's Machine logins page");
    expect((refused as Error).message, name).not.toContain("/credentials/machine");
    expect(await readFile(path, "utf8"), name).toBe(before);
  }
});

test("control: a Dispatch sign-in's refresh that answers its scopes keeps them", async () => {
  const stub = broker({ dispatchScopes: ["person"], scope: "dispatch" });
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url, { dispatch_scopes: ["person"] });
  const refreshed = await refreshDeviceLogin(path, await readDeviceLogin(path), {
    describe: "this machine's device login",
    held: async () => true,
  });
  expect(refreshed.dispatchScopes).toEqual(["person"]);
  expect(JSON.parse(await readFile(path, "utf8")).dispatch_scopes).toEqual(["person"]);
});

test("a machine login's file from before Dispatch sign-ins still refreshes as a machine login", async () => {
  const stub = broker();
  const path = loginPath();
  const written = await writeDeviceLoginFile(path, stub.url);
  const refreshed = await refreshDeviceLogin(path, await readDeviceLogin(path), {
    describe: "this machine's device login",
    held: async () => true,
  });
  expect(refreshed.dispatchScopes).toBeNull();
  expect(stub.refreshes).toHaveLength(1);
  const file = JSON.parse(await readFile(path, "utf8")) as Record<string, unknown>;
  expect(file.refresh_token).not.toBe(written.refreshToken);
  expect(Object.hasOwn(file, "dispatch_scopes")).toBe(false);
});

// A client of the release before this one, on the same machine (claude-envoy and pi-envoy each
// bundle a client, and both default to ~/.config/legion/device-login.json), wrote the file with
// the members it knew, so a Dispatch sign-in's dispatch_scopes are gone from it. The refresh is
// answered under the scope dispatch, which is the answer of a sign-in alone, so the scopes it
// names are kept and saved, checked as a first login's are; the login then reads as the sign-in
// it is, and its end names the sign-in's command, not the machine login's, which must never be
// given to a sign-in's holder (token-errors.ts). The second refresh of the same file is refused
// invalid_grant, as the broker refuses one whose credential was revoked.
test("a file an earlier build's refresh stripped keeps the scopes its refresh answers, and its end names them", async () => {
  const stub = broker({ dispatchScopes: ["person"], refuseRefreshAfter: 1, scope: "dispatch" });
  const path = loginPath();
  await writeDeviceLoginFile(path, stub.url);
  const refreshed = await refreshDeviceLogin(path, await readDeviceLogin(path), {
    describe: "this machine's device login",
    held: async () => true,
  });
  expect(refreshed.dispatchScopes).toEqual(["person"]);
  expect(JSON.parse(await readFile(path, "utf8")).dispatch_scopes).toEqual(["person"]);

  const ended = await refreshDeviceLogin(path, await readDeviceLogin(path), {
    describe: "this Dispatch sign-in",
    held: async () => true,
  }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(ended).toBeInstanceOf(DeviceLoginEndedError);
  expect(ended?.message).toEndWith(`run: ${DEVICE_LOGIN_COMMAND} --scope person`);
});

// Such an answer naming scopes this build may not hold is refused rather than kept as a machine
// login: the file stays as it was and the sign-in stays live. The control is the row above, whose
// answer names a scope a sign-in may hold.
test("a stripped file whose refresh answers scopes no sign-in may hold is refused, and the file stays", async () => {
  for (const scopes of [["approve"], ["person", "person"], "person"]) {
    const stub = broker({ dispatchScopes: scopes as string[], scope: "dispatch" });
    const path = loginPath();
    await writeDeviceLoginFile(path, stub.url);
    const before = await readFile(path, "utf8");
    const name = JSON.stringify(scopes);
    const refused = await refreshDeviceLogin(path, await readDeviceLogin(path), {
      describe: "this machine's device login",
      held: async () => true,
    }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refused, name).toBeInstanceOf(BrokerTradeFailedError);
    expect(refused?.message, name).toContain("Dispatch's Machine logins page");
    expect(await readFile(path, "utf8"), name).toBe(before);
  }
});

// A file whose dispatch_scopes is there but is no list of distinct scopes a sign-in may hold was
// written by no build: it is refused, where a file that holds none reads as a machine login.
test("a login file whose dispatch_scopes is malformed is refused at its read and at its trade", async () => {
  for (const scopes of [["approve"], ["person", "person"], "person", null, [1]]) {
    const stub = broker();
    const path = loginPath();
    await writeDeviceLoginFile(path, stub.url, { dispatch_scopes: scopes });
    const name = JSON.stringify(scopes);
    const read = await readDeviceLogin(path).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(read?.message, name).toContain("dispatch_scopes");
    const traded = await new DeviceLoginIdentity({ path, brokerUrl: stub.url }).read().then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(traded?.message, name).toContain("dispatch_scopes");
    expect(stub.refreshes, name).toHaveLength(0);
    expect(stub.trades, name).toHaveLength(0);
  }
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
      "access_life_seconds",
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
  expect(file.access_life_seconds).toBe(STAND_IN_ACCESS_SECONDS);
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

// The status decides first: a 408 (the broker did not take the poll) and a 429 (it will not take
// one yet) are an outage, as Go's `oauthrefusal.Outage` reads them, so whatever code their body
// names is no person's decision and no lapse of the code, and the poll goes on.
test.each([
  ["429 naming access_denied", "deny", 429],
  ["429 naming expired_token", "expired", 429],
  ["429 naming invalid_grant", "invalid_grant", 429],
  ["408 naming access_denied", "deny", 408],
  ["408 with no body", "empty-408", undefined],
] as const)(
  "a poll answered %s is polled again, and the login completes",
  async (_, answer, status) => {
    const stub = broker({ polls: [answer, "approve"], oauthErrorStatus: status });
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const outcome = await finishDeviceLogin(started, { path: loginPath(), sleep: noSleep }).then(
      (login) => login.credentialId,
      (error: unknown) => (error as Error).message
    );
    expect(outcome).toBe(STAND_IN_CREDENTIAL);
    expect(stub.polls).toHaveLength(2);
  }
);

test("a poll answered 400 authorization_pending, or 429 with no body, is polled again", async () => {
  const stub = broker({ polls: ["pending", "empty-429", "approve"] });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const login = await finishDeviceLogin(started, { path: loginPath(), sleep: noSleep });
  expect(stub.polls).toHaveLength(3);
  expect(login.credentialId).toBe(STAND_IN_CREDENTIAL);
});

// RFC 8628 Section 3.5: each `slow_down` grows the interval by 5 s, at whatever status it comes, as
// the Go helper's `pollLogin` and `openid-client`'s own poll grow it.
test.each([429, 408])("a slow_down answered %i polls again 5 s further apart", async (status) => {
  const stub = broker({ polls: ["slow_down", "approve"], oauthErrorStatus: status });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const waits: number[] = [];
  await finishDeviceLogin(started, {
    path: loginPath(),
    sleep: async (ms) => {
      waits.push(ms);
    },
  });
  expect(stub.polls).toHaveLength(2);
  expect(waits).toEqual([1000, 6000]);
});

// RFC 8628 Section 3.5 has a client poll less often after a connection timeout: a poll the broker
// did not answer, or answered with an outage, polls 5 s further apart from then on, as the Go
// helper's `pollLogin` does. The pending row above is the control: `authorization_pending` keeps
// the interval.
test("each poll the broker did not answer or answered with an outage polls 5 s further apart", async () => {
  const stub = broker({ polls: ["unavailable-503", "approve"] });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const { waits, sleep } = recordedWaits();
  // The first poll never reaches the broker; the second is answered 503; the third is approved.
  let sent = 0;
  const login = await finishDeviceLogin(started, {
    path: loginPath(),
    sleep,
    fetch: (async (input: string | URL | Request, init?: RequestInit) => {
      sent += 1;
      if (sent === 1) throw new TypeError("fetch failed");
      return await fetch(input, init);
    }) as typeof fetch,
  });
  expect(login.credentialId).toBe(STAND_IN_CREDENTIAL);
  expect(stub.polls).toHaveLength(2);
  expect(waits).toEqual([1000, 6000, 11000]);
});

test("a code that lapses after polls the broker did not answer names them", async () => {
  for (const [polls, says] of [
    [
      ["pending", "unavailable-503", "busy-429"],
      "; the broker was unreachable or busy for the last 2 polls;",
    ],
    [
      ["pending", "pending", "empty-408"],
      "; the broker was unreachable or busy for the last poll;",
    ],
    // Control: an answered poll since the outage, and the lapse names none.
    [["unavailable-503", "busy-429", "pending"], null],
  ] as const) {
    const stub = broker({ polls });
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    // The code's lifetime passes on the poll's clock during the wait after the third poll.
    let now = 0;
    let waited = 0;
    const refusal = await finishDeviceLogin(started, {
      path: loginPath(),
      clock: () => now,
      sleep: async () => {
        waited += 1;
        if (waited === 4) now += 3_600_000;
      },
    }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(stub.polls, polls.join()).toHaveLength(3);
    expect(refusal?.message, polls.join()).toContain(
      `the code ${STAND_IN_USER_CODE} expired before ${PERSON} approved this machine`
    );
    expect(refusal?.message, polls.join()).toContain(DEVICE_LOGIN_COMMAND);
    if (says === null) expect(refusal?.message, polls.join()).not.toContain("unreachable");
    else expect(refusal?.message, polls.join()).toContain(says);
  }
});

// A wait never passes the code's lifetime: with 2 s of it left, a 503 that grows the interval to
// 6 s waits what is left, and a code that lapses during that wait is not polled again, and names
// the outage before it. Each wait passes on the poll's clock.
test("a poll's wait ends at the code's lifetime, and a lapse in it names the outages before", async () => {
  const stub = broker({ polls: ["unavailable-503", "approve"] });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const { waits, sleep, clock } = advancingWaits();
  const refusal = await finishDeviceLogin(
    { ...started, expiresAt: new Date(Date.now() + 2000) },
    { path: loginPath(), clock, sleep }
  ).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(waits[0]).toBe(1000);
  expect(waits[1] ?? 0).toBeLessThanOrEqual(1000);
  expect(stub.polls).toHaveLength(1);
  expect(refusal?.message).toContain("; the broker was unreachable or busy for the last poll;");
});

// An outage's `Retry-After` is waited before the next poll where it asks longer than the interval.
// The broker's own 429 on its token endpoint asks 1 s (`handlers_oauth.go`: its token limit refills
// one request every 500 ms, which `Retry-After` rounds up); a proxy before it may ask longer, and
// 30 s is such a value.
test("a poll answered 429 with Retry-After: 30 waits 30 s before the next poll", async () => {
  const stub = broker({ polls: ["busy-429", "approve"], retryAfter: "30" });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const { waits, sleep } = recordedWaits();
  await finishDeviceLogin(started, { path: loginPath(), sleep });
  expect(stub.polls).toHaveLength(2);
  expect(waits).toEqual([1000, 30_000]);
});

test("a poll answered 429 with a Retry-After date waits until that date", async () => {
  // A date in whole seconds, as an HTTP date names it.
  const at = Math.ceil((Date.now() + 30_000) / 1000) * 1000;
  const stub = broker({ polls: ["busy-429", "approve"], retryAfter: new Date(at).toUTCString() });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const { waits, sleep } = recordedWaits();
  // The wait is read between the poll's start and its end, so no clock decides the row.
  const began = Date.now();
  await finishDeviceLogin(started, { path: loginPath(), sleep });
  const ended = Date.now();
  expect(waits).toHaveLength(2);
  expect(waits[0]).toBe(1000);
  expect(waits[1] ?? 0).toBeLessThanOrEqual(at - began);
  expect(waits[1] ?? 0).toBeGreaterThanOrEqual(at - ended);
});

// The poll's clock moves by each wait, so the second wait, the 429's day cut at the code's end,
// lapses the code, and no poll goes out after it.
test("a Retry-After past the code's lifetime is waited until the code lapses, and polls no more", async () => {
  const stub = broker({ polls: ["busy-429", "approve"], retryAfter: "86400" });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const { waits, sleep, clock } = advancingWaits();
  const began = Date.now();
  const refusal = await finishDeviceLogin(started, { path: loginPath(), clock, sleep }).then(
    () => null,
    (error: unknown) => error as Error
  );
  const ended = Date.now();
  // The code's end is what was left of it as the poll began, read between `began` and `ended`.
  expect(waits).toHaveLength(2);
  expect(waits[0]).toBe(1000);
  expect(waits[1] ?? 0).toBeLessThanOrEqual(started.expiresAt.getTime() - began - 1000);
  expect(waits[1] ?? 0).toBeGreaterThanOrEqual(started.expiresAt.getTime() - ended - 1000);
  expect(stub.polls).toHaveLength(1);
  expect(refusal?.message).toContain("; the broker was unreachable or busy for the last poll;");
});

// The code's end is the hour at most, whatever `started` names: a step back of the wall clock
// between `startDeviceLogin` and the poll, or a caller's own `started`, cannot make a wait pass it.
test("a code the poll is handed past the hour is waited an hour at most", async () => {
  const stub = broker({ polls: ["busy-429", "approve"], retryAfter: "86400" });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const { waits, sleep, clock } = advancingWaits();
  const refusal = await finishDeviceLogin(
    { ...started, expiresAt: new Date(Date.now() + 2 * 3_600_000) },
    { path: loginPath(), clock, sleep }
  ).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(waits).toEqual([1000, 3_599_000]);
  expect(stub.polls).toHaveLength(1);
  expect(refusal?.message).toContain(`the code ${STAND_IN_USER_CODE} expired before ${PERSON}`);
});

// The device code is held for the scrub until an hour past the poll's end, read again on the wall
// clock at each poll: a step of that clock past the code's life and the hour, and another holder's
// hold, which forgets every value whose time has come, leave it held while it is still polled.
test("the device code is held for the scrub while it is polled, whatever the wall clock reads", async () => {
  const stub = broker({ polls: ["pending", "approve"] });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const scrubbed: string[] = [];
  let waited = 0;
  try {
    await finishDeviceLogin(started, {
      path: loginPath(),
      sleep: async () => {
        waited += 1;
        if (waited !== 2) return;
        setSystemTime(new Date(Date.now() + 3 * 3_600_000));
        holdSecret(fixtureSecret("other"));
      },
      fetch: (async (input: string | URL | Request, init?: RequestInit) => {
        scrubbed.push(scrub(started.deviceCode));
        // Put back before the stand-in answers, since its answer and the login are dated.
        setSystemTime();
        return await fetch(input, init);
      }) as typeof fetch,
    });
  } finally {
    setSystemTime();
  }
  expect(stub.polls).toHaveLength(2);
  for (const text of scrubbed) expect(text).not.toContain(started.deviceCode);
});

// The code's end is kept on the monotonic clock (`performance.now()`): a step of the wall clock
// past the code's lifetime, as NTP or a person setting the clock makes, neither ends the poll early
// nor, back, keeps it going past the code. The wall clock is put back before the save, which dates
// the login. `performance.now()` is spied on, not waited out, so the second row's wait passes on it
// at once.
test("a step of the wall clock neither ends the poll early nor keeps it past the code", async () => {
  const twoHours = 2 * 3_600_000;
  const monotonic = performance.now.bind(performance);
  let ahead = 0;
  const spied = spyOn(performance, "now").mockImplementation(() => monotonic() + ahead);
  try {
    const forward = broker({ polls: ["pending", "approve"] });
    const started = await startDeviceLogin({ brokerUrl: forward.url, person: PERSON, host: HOST });
    let waited = 0;
    const login = await finishDeviceLogin(started, {
      path: loginPath(),
      sleep: async () => {
        waited += 1;
        if (waited === 1) setSystemTime(new Date(Date.now() + twoHours));
        else setSystemTime();
      },
    });
    expect(login.credentialId).toBe(STAND_IN_CREDENTIAL);
    expect(forward.polls).toHaveLength(2);

    // A code with 10 s left, polled every second, lapses on the monotonic clock in its tenth wait,
    // though the wall clock stepped back two hours in each wait; the stand-in would approve the
    // twenty-first poll.
    const back = broker({ polls: [...Array<"pending">(20).fill("pending"), "approve"] });
    const backStarted = await startDeviceLogin({ brokerUrl: back.url, person: PERSON, host: HOST });
    const refusal = await finishDeviceLogin(
      { ...backStarted, expiresAt: new Date(Date.now() + 10_000) },
      {
        path: loginPath(),
        sleep: async (ms) => {
          setSystemTime(new Date(Date.now() - twoHours));
          ahead += ms;
        },
      }
    ).then(
      () => null,
      (error: unknown) => error as Error
    );
    // The tenth wait lapses the code, so nine polls at most; ten would be a poll after the lapse.
    expect(ahead).toBeGreaterThan(0);
    expect(back.polls.length).toBeLessThanOrEqual(9);
    expect(refusal?.message).toContain(`the code ${STAND_IN_USER_CODE} expired before ${PERSON}`);
  } finally {
    setSystemTime();
    spied.mockRestore();
  }
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

// A `started` handed to the poll is checked again: one a caller made, or one the time since the
// start, or a step of the wall clock, has brought nearer its end, may lapse no later than its first
// poll, or have lapsed. It is refused at once, naming the code, not waited out and said to have
// expired before the person approved it; asking again starts a code `startDeviceLogin` checks.
test("a code handed to the poll that lapses no later than its first poll is refused at once", async () => {
  const stub = broker();
  // The wall clock stands still, so each row's time left is what the row set, to the millisecond.
  const now = Date.now();
  const still = spyOn(Date, "now").mockReturnValue(now);
  try {
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    for (const [handed, says] of [
      [
        { ...started, intervalSeconds: 1_800 },
        /^lapses in 600 s, no later than its first poll, 1800 s on/,
      ],
      // A fraction of a second is printed as it is, so the time left reads shorter than the wait.
      [
        { ...started, intervalSeconds: 4.5, expiresAt: new Date(now + 4_200) },
        /^lapses in 4\.2 s, no later than its first poll, 4\.5 s on/,
      ],
      // The first poll is named by the wait the test decided on: 4.9997 s waits 5 s, which a code
      // that lapses in 5 s does not outlast. An interval with no digits past the millisecond is
      // printed in its own digits, however long, not as milliseconds a number cannot hold.
      [
        { ...started, intervalSeconds: 4.9997, expiresAt: new Date(now + 5_000) },
        /^lapses in 5 s, no later than its first poll, 5 s on/,
      ],
      [
        { ...started, intervalSeconds: Number.MAX_VALUE },
        /^lapses in 600 s, no later than its first poll, 17976931348623157\d{292} s on/,
      ],
      [
        { ...started, expiresAt: new Date(now - 1_000) },
        /^had lapsed before its first poll, 1 s on/,
      ],
      [
        { ...started, intervalSeconds: 5_000, expiresAt: new Date(now + 7_200_000) },
        /^would lapse here before its first poll, 5000 s on, as this machine polls a device code for an hour at most/,
      ],
    ] as const) {
      const { waits, sleep } = recordedWaits();
      const refusal = await finishDeviceLogin(handed, { path: loginPath(), sleep }).then(
        () => null,
        (error: unknown) => error as Error
      );
      const message = refusal?.message ?? "";
      expect(message, String(says)).toStartWith(`the code ${STAND_IN_USER_CODE} `);
      expect(message.slice(`the code ${STAND_IN_USER_CODE} `.length), String(says)).toMatch(says);
      expect(message, String(says)).toEndWith(
        `, so no poll could reach it; ask again with: ${DEVICE_LOGIN_COMMAND}`
      );
      expect(waits, String(says)).toHaveLength(0);
    }
  } finally {
    still.mockRestore();
  }
  expect(stub.polls).toHaveLength(0);
});

// A `started` a caller made whose end is no date, or whose interval is no number of seconds, 1 or
// more, would poll the broker as fast as it answers: it is refused before any poll.
test("a code handed to the poll with no date or no interval of a second or more is refused", async () => {
  const stub = broker();
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  for (const handed of [
    { ...started, intervalSeconds: Number.NaN },
    { ...started, intervalSeconds: 0 },
    { ...started, expiresAt: new Date(Number.NaN) },
  ]) {
    const { waits, sleep } = recordedWaits();
    const refusal = await finishDeviceLogin(handed, { path: loginPath(), sleep }).then(
      () => null,
      (error: unknown) => error
    );
    expect(refusal).toBeInstanceOf(TypeError);
    expect(waits).toHaveLength(0);
  }
  expect(stub.polls).toHaveLength(0);
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
  expect(login.accessLifeSeconds).toBe(hour / 1000);
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
  // Denied, expired, and a code the broker no longer knows (`invalid_grant`), which is an expiry,
  // each at the broker's own status: `access_denied` 403, as its OAuth library answers it, and
  // the others 400.
  for (const answer of ["deny", "expired", "invalid_grant"] as const) {
    const stub = broker({ polls: [answer] });
    const path = loginPath();
    const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
    const refusal = await finishDeviceLogin(started, { path, sleep: noSleep }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(stub.polls, answer).toHaveLength(1);
    // The denial's own words: a refusal naming `HTTP 403 access_denied` holds "denied" too.
    expect(refusal?.message, answer).toContain(
      answer === "deny" ? `denied, not approved as ${PERSON}` : "expired before"
    );
    expect(refusal?.message).toContain(DEVICE_LOGIN_COMMAND);
    expect(await stat(path).catch(() => null)).toBeNull();
  }
});

// Go's `oauthrefusal.Refusal`, which the helper's `pollLogin` reads: RFC 6749 Section 5.2 has an
// error answer be 400 unless specified otherwise, and an OAuth library may specify otherwise.
test.each([
  ["access_denied at 400", "deny", 400],
  ["access_denied at 401", "deny", 401],
  ["expired_token at 403", "expired", 403],
  ["invalid_grant at 403", "invalid_grant", 403],
] as const)("a poll's OAuth error counts at any refusal status: %s", async (_, answer, status) => {
  const stub = broker({ polls: [answer], oauthErrorStatus: status });
  const started = await startDeviceLogin({ brokerUrl: stub.url, person: PERSON, host: HOST });
  const refusal = await finishDeviceLogin(started, { path: loginPath(), sleep: noSleep }).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(stub.polls).toHaveLength(1);
  expect(refusal?.message).toContain(
    answer === "deny" ? `denied, not approved as ${PERSON}` : "expired before"
  );
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

// A session started before another login of this machine is told its one cure: start a new
// session. The broker keeps a session name for the principal that wrote under it; a Dispatch
// sign-in is a principal of its own, and so is each person's machine login, and the file keeps no
// person, so the client cannot tell the one case a restart would cure. Each case, a machine login
// of another person over a machine login among them, gets the same words. That case is not a row
// of its own: the file holds no person (`device-login-file.ts`), so its file is the same as the
// first row's. No later read finds this process's key again, so the refusal is final in each case.
test("a session started before another login of this machine refuses to trade the new one", async () => {
  const machine = {};
  const signIn = { dispatch_scopes: ["person"] };
  const cure = `${DEVICE_LOGIN_FILE_NAMED} holds another login than the one this process started with, since ${DEVICE_LOGIN_COMMAND} ran again; start a new session to trade the new one: the secrets broker keeps a session's name for the identity that wrote under it, and the login file does not say whose the new login is, so this session started again may be refused`;
  for (const [name, started, now] of [
    ["a machine login, then a machine login", machine, machine],
    ["a machine login, then a sign-in", machine, signIn],
    ["a sign-in, then a machine login", signIn, machine],
    ["a sign-in, then a sign-in", signIn, signIn],
  ] as const) {
    const stub = broker();
    const path = loginPath();
    const live = {
      access_token: fixtureSecret("access"),
      access_expires_at: new Date(Date.now() + 600_000).toISOString(),
    };
    await writeDeviceLoginFile(path, stub.url, { ...live, ...started });
    const identity = new DeviceLoginIdentity({ path, brokerUrl: stub.url });
    await identity.key();
    // `dispatch login --device` ran again since: the file holds a login of another key, which the
    // client this session made does not prove.
    await writeDeviceLoginFile(path, stub.url, { ...live, ...now });
    const refusal = await identity
      .spend(async () => {})
      .then(
        () => null,
        (error: unknown) => error as Error
      );
    expect(refusal, name).toBeInstanceOf(DeviceLoginReplacedError);
    expect(isFinalRefusal(refusal), name).toBe(true);
    expect(refusal?.message, name).toBe(cure);
    expect(stub.refreshes, name).toHaveLength(0);
  }
});

// The token client's retry loop ends on that refusal: no later read finds this process's key
// again, so a retry is a trade no wait cures. The session's one trade answers the caller with the
// refusal and its cure, logs no "trading again", tells no caller the trade is being retried, and
// a second call before the loop's next slot is answered the same with no second trade.
test("a session whose login file another login replaced stops trading at once and says what to start", async () => {
  const stub = broker();
  const path = loginPath();
  const live = {
    access_token: fixtureSecret("access"),
    access_expires_at: new Date(Date.now() + 600_000).toISOString(),
  };
  await writeDeviceLoginFile(path, stub.url, live);
  const env = {
    [BROKER_URL_VAR]: stub.url,
    [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
    [DEVICE_LOGIN_FILE_VAR]: path,
  };
  // The client reads the login's key when it is made, as a session's client is.
  const made = await brokerTokenClient(env, "ses-laptop-1");
  await writeDeviceLoginFile(path, stub.url, { ...live, dispatch_scopes: ["person"] });
  const spends = spyOn(DeviceLoginIdentity.prototype, "spend");
  const warned: string[] = [];
  const warn = spyOn(console, "warn").mockImplementation((...line: unknown[]) => {
    warned.push(line.join(" "));
  });
  try {
    const refusal = await made?.client.token().then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refusal).toBeInstanceOf(DeviceLoginReplacedError);
    expect(refusal?.message).toContain("start a new session");
    expect(refusal?.message).not.toContain("being retried");
    const again = await made?.client.token().then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(again).toBe(refusal);
    expect(spends).toHaveBeenCalledTimes(1);
    expect(warned.filter((line) => line.includes("trading again"))).toEqual([]);
    expect(stub.trades).toHaveLength(0);
  } finally {
    made?.client.close();
    spends.mockRestore();
    warn.mockRestore();
  }
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
    // A restricted member's machine login naming no project or several, which the broker's
    // exchange policy holds to exactly one (`exchange.ErrRestrictedOneProject`).
    [
      "invalid_request",
      sentAs(
        "is a restricted member, who sees only the projects that name them, and the trade asked for 2; set AGENT_SECRETS_PROJECTS to the one project this session works on."
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

// A Dispatch sign-in that ended is cured by a new Dispatch sign-in of the scopes it held: the
// machine login's command would make a login that starts sessions and reads secrets. Its file's
// dispatch_scopes name them, `none` where it held none; a machine login's file names the
// machine login's command, as before.
test("an ended login names the command that makes the same kind of login again", async () => {
  for (const [name, fields, says] of [
    [
      "a person sign-in",
      { dispatch_scopes: ["person"] },
      `run: ${DEVICE_LOGIN_COMMAND} --scope person`,
    ],
    ["an empty sign-in", { dispatch_scopes: [] }, `run: ${DEVICE_LOGIN_COMMAND} --scope none`],
    ["a machine login", {}, `run: ${DEVICE_LOGIN_COMMAND}`],
  ] as const) {
    const stub = broker({ refuseRefresh: true });
    const path = loginPath();
    await writeDeviceLoginFile(path, stub.url, fields);
    const refused = await refreshDeviceLogin(path, await readDeviceLogin(path), {
      describe: "this machine's device login",
      held: async () => true,
    }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refused, name).toBeInstanceOf(DeviceLoginEndedError);
    expect(refused?.message, name).toEndWith(says);
  }
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

// Only a 400 or a 401 `invalid_grant` ends a grant (Go's `oauthrefusal.EndsGrant`). At a 403 it is
// the broker's refusal, worded as every other is; at a 429 it is the broker's rate limit, which a
// later refresh gets past.
test.each([
  [400, true],
  [401, true],
  [403, false],
  [429, false],
  [408, false],
] as const)("a refresh answered %i invalid_grant ends the login: %p", async (status, ends) => {
  const stub = broker({ refuseRefresh: true, oauthErrorStatus: status });
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
  if (ends) {
    expect(refused).toBeInstanceOf(DeviceLoginEndedError);
    expect(refused?.message).toContain(DEVICE_LOGIN_COMMAND);
    expect(isFinalRefusal(refused)).toBe(true);
    return;
  }
  expect(refused).not.toBeInstanceOf(DeviceLoginEndedError);
  expect(refused).toBeInstanceOf(BrokerRefusedError);
  expect(refused?.message).toContain(
    `the secrets broker refused this machine's device login: HTTP ${status} invalid_grant`
  );
  expect(isFinalRefusal(refused)).toBe(status === 403);
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
