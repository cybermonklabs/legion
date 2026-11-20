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

test("of two callers that find one stale lock, exactly one takes it", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  await writeFile(lock, "a process that died holding it");
  const old = new Date(Date.now() - 31_000);
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

test("a take-over waits while another caller takes over, and not for one that died doing so", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  const steal = `${lock}.steal`;
  const old = new Date(Date.now() - 31_000);
  await writeFile(lock, "a process that died holding it");
  await utimes(lock, old, old);

  // Another caller is taking the stale lock over: this one waits, and runs once that caller's
  // take-over lock is gone, at its third wait.
  await writeFile(steal, "another caller taking over");
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
        if (waits === 3) await Bun.file(steal).delete();
      },
    }
  );
  expect(ranAfter).toBe(3);

  // A take-over lock 6 s old is one whose caller died inside its take-over: it is removed, and
  // the stale lock taken, after one wait.
  await writeFile(lock, "a process that died holding it");
  await utimes(lock, old, old);
  await writeFile(steal, "a caller that died taking over");
  const stealOld = new Date(Date.now() - 6_000);
  await utimes(steal, stealOld, stealOld);
  let deadWaits = 0;
  await withDeviceLoginLock(path, async () => {}, {
    sleep: async () => {
      deadWaits += 1;
    },
  });
  expect(deadWaits).toBe(1);
  expect(await stat(steal).catch(() => null)).toBeNull();
});

// A file another caller makes in place of one this caller found stale can carry the very inode
// number of the one it replaced: XFS hands a new file the number freed a moment before (5 of 5
// tries on a review box). These rows write the other caller's file into the stale file's own
// inode, which is exactly what such a filesystem gives the check to see, so they hold on every
// filesystem: the same device and inode, and a fresh modification time and holder value.

test("of two callers inside one take-over, the one that comes second puts the lock back", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  await writeFile(lock, "a process that died holding it");
  const old = new Date(Date.now() - 31_000);
  await utimes(lock, old, old);

  // A is under its own take-over lock, has found the lock still stale, and is held before it
  // moves the lock aside: it stopped there for over 5 s, so B took A's take-over lock as dead,
  // took the stale lock over and now holds a fresh one. A then goes on. It must find the lock it
  // moves is B's, put it back and wait for B, never take it from B.
  const aTaking = Promise.withResolvers<void>();
  const aGoes = Promise.withResolvers<void>();
  let bHolds = false;
  let heldAtFirstWait = "";
  let ranWhileBHeld: boolean | undefined;
  const a = withDeviceLoginLock(
    path,
    async () => {
      ranWhileBHeld = bHolds;
    },
    {
      at: async (step) => {
        if (step !== "taking") return;
        aTaking.resolve();
        await aGoes.promise;
      },
      sleep: async () => {
        if (heldAtFirstWait === "") {
          heldAtFirstWait = await readFile(lock, "utf8").catch(() => "(no lock)");
        }
        // B's work ends, and B lets go.
        bHolds = false;
        await Bun.file(lock).delete();
      },
    }
  );
  await aTaking.promise;
  await writeFile(lock, "B holds it");
  bHolds = true;
  aGoes.resolve();
  await a;
  expect(heldAtFirstWait).toBe("B holds it");
  expect(ranWhileBHeld).toBe(false);
  expect(await stat(lock).catch(() => null)).toBeNull();
});

test("a dead take-over lock is removed only while it is still the dead one", async () => {
  const path = loginPath();
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const lock = `${path}.lock`;
  const steal = `${lock}.steal`;
  const old = new Date(Date.now() - 31_000);
  await writeFile(lock, "a process that died holding it");
  await utimes(lock, old, old);
  await writeFile(steal, "a caller that died taking over");
  const stealOld = new Date(Date.now() - 6_000);
  await utimes(steal, stealOld, stealOld);

  // B finds the take-over lock dead and is held before it removes it, while C removes that dead
  // one and starts a take-over of its own. B then goes on: it must leave C's take-over lock in
  // place, and wait for C.
  const bFound = Promise.withResolvers<void>();
  const bGoes = Promise.withResolvers<void>();
  let stealAtFirstWait = "";
  const b = withDeviceLoginLock(path, async () => {}, {
    at: async (step) => {
      if (step !== "steal-stale" || stealAtFirstWait !== "") return;
      bFound.resolve();
      await bGoes.promise;
    },
    sleep: async () => {
      if (stealAtFirstWait === "") {
        stealAtFirstWait = await readFile(steal, "utf8").catch(() => "(no take-over lock)");
      }
      // C's take-over ends.
      await Bun.file(steal).delete();
    },
  });
  await bFound.promise;
  await writeFile(steal, "C takes over");
  bGoes.resolve();
  await b;
  expect(stealAtFirstWait).toBe("C takes over");
  expect(await stat(steal).catch(() => null)).toBeNull();
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
