// packages/cipher-client/src/__tests__/environment.test.ts
//
// What a deployment's variables name, and what a deployment that names half of one is told.

import { afterEach, expect, test } from "bun:test";
import { createPublicKey, generateKeyPairSync } from "node:crypto";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { calculateJwkThumbprint, type JWK } from "jose";
import {
  type DeviceLoginBroker,
  stubDeviceLoginBroker,
  writeDeviceLoginFile,
} from "../../test/device-logins";
import { DeviceLoginIdentity, deviceLoginSessionName, startDeviceLogin } from "../device-login";
import { DEVICE_LOGIN_FILE_VAR, deviceLoginFile } from "../device-login-file";
import {
  BROKER_URL_VAR,
  brokerTokenClient,
  IDENTITY_DEVICE_LOGIN,
  IDENTITY_VAR,
  KEY_DIR_VAR,
  MACHINE_AUDIENCE_VAR,
  MACHINE_TOKEN_FILE_VAR,
  MAX_PROJECTS,
  machineIdentity,
  PROJECTS_VAR,
  ROLE_KEY_FILE,
  SESSION_VAR,
  tradeProjects,
  tradeSession,
} from "../environment";
import { PERSON_TRADE_VAR } from "../signins";
import {
  FORM_AS_OF,
  FORM_LAUNCHER_CREDENTIAL,
  FORM_LAUNCHER_CREDENTIAL_DECLINED,
  FORM_PROJECTS,
  FORM_SCOPE,
  SCOPE_READ,
} from "../wire";
import { accepted, refusalOf, stubBroker } from "./stub";

const makeDirectory = temporaryDirectories("broker-environment");

const PERSON = "ada@example.com";
const HOST = "adas-laptop.local";

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A device login's stand-in broker, stopped after the test. */
function broker(): DeviceLoginBroker {
  const stub = stubDeviceLoginBroker();
  stops.push(stub.stop);
  return stub;
}

/** A device login file's path in a directory of this test's own. */
function loginPath(): string {
  return join(makeDirectory(), "legion", "device-login.json");
}

test("a machine that names no identity trades nothing, and that is no error", async () => {
  expect(machineIdentity({})).toBeUndefined();
  expect(machineIdentity({ [BROKER_URL_VAR]: "http://broker.test" })).toBeUndefined();
  expect(await brokerTokenClient({ [BROKER_URL_VAR]: "http://broker.test" })).toBeUndefined();
  // A named identity with no broker to spend it at trades nothing either.
  expect(
    await brokerTokenClient({
      [IDENTITY_VAR]: "kubernetes",
      [MACHINE_TOKEN_FILE_VAR]: "/var/run/secrets/token",
    })
  ).toBeUndefined();
});

test("each identity is read from its own variables", () => {
  const pod = machineIdentity({
    [IDENTITY_VAR]: "kubernetes",
    [MACHINE_TOKEN_FILE_VAR]: "/var/run/secrets/exchange/token",
  });
  expect(pod?.describe()).toBe(
    "the projected service-account token AGENT_SECRETS_MACHINE_TOKEN_FILE names"
  );
  const machine = machineIdentity({
    [IDENTITY_VAR]: "metadata",
    [MACHINE_AUDIENCE_VAR]: "example-audience",
  });
  expect(machine?.describe()).toContain("example-audience");
});

test("an identity named with its variables missing is refused naming them", () => {
  expect(() => machineIdentity({ [IDENTITY_VAR]: "kubernetes" })).toThrow(
    `${MACHINE_TOKEN_FILE_VAR} is required by ${IDENTITY_VAR}=kubernetes`
  );
  expect(() => machineIdentity({ [IDENTITY_VAR]: "metadata" })).toThrow(
    `${MACHINE_AUDIENCE_VAR} is required by ${IDENTITY_VAR}=metadata`
  );
  // A pool federates the projected token file, so it is the first variable it asks for; each of
  // its four is held to its own refusal in pool-identity.test.ts.
  expect(() => machineIdentity({ [IDENTITY_VAR]: "pool" })).toThrow(
    `${MACHINE_TOKEN_FILE_VAR} is required by ${IDENTITY_VAR}=pool`
  );
  // Its value is not quoted, which a secret pasted into the wrong variable would be.
  const pasted = "canary-pasted-identity-secret";
  let refused = "";
  try {
    machineIdentity({ [IDENTITY_VAR]: pasted });
  } catch (error) {
    refused = error instanceof Error ? error.message : String(error);
  }
  expect(refused).toBe(
    "AGENT_SECRETS_MACHINE_IDENTITY names no identity this machine can prove; it takes kubernetes, metadata, pool or device-login"
  );
  expect(refused).not.toContain(pasted);
});

// AGENT_SECRETS_URL is where every trade goes, and every trade's error names it; a value that is
// no http or https URL is refused naming the variable, since it may be a token pasted there.
const CANARY_PW = "canary-pw-7c1e";
test.each([
  "canary-pasted-into-the-broker-url",
  "nats://broker.example:4222",
  `https://u:${CANARY_PW}@broker.example:99999`,
])("an AGENT_SECRETS_URL of %p is refused naming the variable alone", async (pasted) => {
  const refusal = await brokerTokenClient({
    [BROKER_URL_VAR]: pasted,
    [IDENTITY_VAR]: "kubernetes",
    [MACHINE_TOKEN_FILE_VAR]: "/var/run/secrets/exchange/token",
    AGENT_SECRETS_SIGNIN_DIR: join(makeDirectory(), "signins"),
  }).then(
    () => undefined,
    (error: unknown) => error
  );
  if (!(refusal instanceof Error)) throw new Error("the factory made a client");
  expect(refusal.message).toBe(`${BROKER_URL_VAR} is not an http or https URL`);
  expect(Bun.inspect(refusal)).not.toContain(pasted);
});

test("a client is built for the broker, the identity and the session the variables name", async () => {
  const made = await brokerTokenClient({
    [BROKER_URL_VAR]: "http://broker.test/",
    [IDENTITY_VAR]: "kubernetes",
    [MACHINE_TOKEN_FILE_VAR]: "/var/run/secrets/exchange/token",
    AGENT_SECRETS_SESSION: "example-worker-session",
    // Nobody has signed in here, so the client trades the machine (person-route.test.ts holds
    // the client a sign-in makes).
    AGENT_SECRETS_SIGNIN_DIR: join(makeDirectory(), "signins"),
  });
  if (made === undefined) throw new Error("the factory made no client for a projected token");
  const { client } = made;
  expect(client.describe()).toBe(
    "the projected service-account token AGENT_SECRETS_MACHINE_TOKEN_FILE names"
  );
  // The key is this process's own, made once with the client, and only its public half exists
  // outside the signer.
  expect(Object.hasOwn(client.key.publicJwk, "d")).toBe(false);
});

test("a machine's client this factory builds asks the broker for no launcher credential", async () => {
  // Every TypeScript caller wants a Dispatch access token and nothing else, and a machine's key
  // is one process's and one session's: a trade that asked would leave a launcher_credentials
  // row and an audit row behind for each `dispatch` call, hook run and session, on the
  // operator's machine-logins list and removed by nothing.
  const forms: URLSearchParams[] = [];
  const broker = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      forms.push(new URLSearchParams(await request.text()));
      return Response.json({
        access_token: fixtureSecret("broker"),
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  try {
    const file = join(makeDirectory(), "token");
    await writeFile(file, `${fixtureSecret("proj")}\n`);
    const made = await brokerTokenClient({
      [BROKER_URL_VAR]: `http://127.0.0.1:${broker.port}`,
      [IDENTITY_VAR]: "kubernetes",
      [MACHINE_TOKEN_FILE_VAR]: file,
      AGENT_SECRETS_SESSION: "example-worker-session",
      AGENT_SECRETS_SIGNIN_DIR: join(makeDirectory(), "signins"),
      // A machine trading for itself names no project, whatever the variable says: its entry
      // decides its project, and the broker refuses one it is told.
      [PROJECTS_VAR]: "PRIV",
    });
    if (made === undefined) throw new Error("the factory made no client for a projected token");
    const token = await made.client.token();
    made.client.close();
    expect(token.credentialId).toBe("");
    expect(forms).toHaveLength(1);
    expect(forms[0]?.get(FORM_LAUNCHER_CREDENTIAL)).toBe(FORM_LAUNCHER_CREDENTIAL_DECLINED);
    expect(forms[0]?.has(FORM_PROJECTS)).toBe(false);
  } finally {
    broker.stop(true);
  }
});

/** A role's environment in a pod whose agent-secrets directory is `keyDir`: the kubernetes
 *  identity, a projected token file, and nobody signed in. */
async function podRole(brokerUrl: string, keyDir: string): Promise<Record<string, string>> {
  const token = join(makeDirectory(), "token");
  await writeFile(token, `${fixtureSecret("role")}\n`);
  return {
    [BROKER_URL_VAR]: brokerUrl,
    [IDENTITY_VAR]: "kubernetes",
    [MACHINE_TOKEN_FILE_VAR]: token,
    [KEY_DIR_VAR]: keyDir,
    AGENT_SECRETS_SESSION: "implementer-g1",
    AGENT_SECRETS_SIGNIN_DIR: join(makeDirectory(), "signins"),
  };
}

test("a pod role trades with its own agent-secrets key", async () => {
  // The key `agent-secrets keygen` writes: P-256, PKCS#8 PEM, 0600. The broker holds the pod
  // session's exchange to this key's thumbprint, which the role's registration names.
  const keyDir = makeDirectory();
  const { privateKey } = generateKeyPairSync("ec", { namedCurve: "P-256" });
  const pem = privateKey.export({ format: "pem", type: "pkcs8" }).toString();
  await writeFile(join(keyDir, ROLE_KEY_FILE), pem, { mode: 0o600 });
  const { kty, crv, x, y } = createPublicKey(pem).export({ format: "jwk" });
  const broker = stubBroker(() => ({ body: accepted("role-access-token", 600) }));
  try {
    const made = await brokerTokenClient(await podRole(broker.url, keyDir));
    await made?.client.token();
    expect(broker.trades).toHaveLength(1);
    const proved = await calculateJwkThumbprint(broker.trades[0]?.proofHeader.jwk as JWK);
    expect(proved).toBe(await calculateJwkThumbprint({ kty, crv, x, y } as JWK));
  } finally {
    broker.stop();
  }
});

test("a role key that cannot be read or parsed refuses the trade, naming AGENT_SECRETS_KEY_DIR and not the path", async () => {
  // The trade refuses rather than proving a fresh key, which the broker would refuse in words
  // that name neither the file nor the key. The refusal names the directory by its variable and
  // why, never the path, which can be a token a person pasted where the directory goes, and it
  // keeps no cause, since the runtime's error repeats the path it was handed.
  const pasted = "madeUpKeyDirToken0123456789abcdef";
  const root = makeDirectory();
  const notPem = join(root, "not-pem");
  await mkdir(notPem);
  await writeFile(join(notPem, ROLE_KEY_FILE), `${fixtureSecret("notkey")}\n`, { mode: 0o600 });
  const broker = stubBroker(() => ({ body: accepted("role-access-token", 600) }));
  try {
    for (const [dir, want] of [
      [
        join(root, pasted),
        `read this role's agent-secrets key in the directory ${KEY_DIR_VAR} names: ENOENT`,
      ],
      [
        notPem,
        `read this role's agent-secrets key in the directory ${KEY_DIR_VAR} names: it does not parse as a PKCS#8 PEM private key`,
      ],
    ] as const) {
      const refusal = await refusalOf(async () =>
        brokerTokenClient(await podRole(broker.url, dir))
      );
      expect(refusal).toBeInstanceOf(Error);
      expect(refusal?.message).toBe(want);
      expect(refusal?.cause).toBeUndefined();
      expect(refusal?.message).not.toContain(root);
      for (let at = 0; at + 10 <= pasted.length; at += 1) {
        expect(refusal?.message).not.toContain(pasted.slice(at, at + 10));
      }
    }
    expect(broker.trades).toHaveLength(0);
  } finally {
    broker.stop();
  }
});

test("the projects a person's trade names are AGENT_SECRETS_PROJECTS, read by the broker's grammar", () => {
  expect(tradeProjects({})).toEqual([]);
  expect(tradeProjects({ [PROJECTS_VAR]: "  " })).toEqual([]);
  expect(tradeProjects({ [PROJECTS_VAR]: "PUB  PRIV" })).toEqual(["PRIV", "PUB"]);
  const seventeen = Array.from(
    { length: MAX_PROJECTS + 1 },
    (_, at) => `P${String(at).padStart(2, "0")}`
  );
  for (const [bad, says] of [
    ["priv", '"priv" is not a project key'],
    ["PRIV,PUB", '"PRIV,PUB" is not a project key'],
    ["P", '"P" is not a project key'],
    ["PRIV PUB PRIV", "PRIV is named twice"],
    [seventeen.join(" "), "at most 16"],
  ] as const) {
    expect(() => tradeProjects({ [PROJECTS_VAR]: bad })).toThrow(says);
    expect(() => tradeProjects({ [PROJECTS_VAR]: bad })).toThrow(PROJECTS_VAR);
  }
});

test("the session a trade writes under is the caller's own, and the variable's where it has none", () => {
  const deployment = { AGENT_SECRETS_SESSION: " legion-demo-controller " };
  // A caller that acts as a session: its token must name that session, since the Envoy
  // listener refuses a write acting as one whose token names another.
  expect(tradeSession(deployment, "ses_01a9")).toBe("ses_01a9");
  expect(tradeSession({}, " ses_01a9 ")).toBe("ses_01a9");
  // A caller that acts as no session: a daemon, a command, the helper.
  expect(tradeSession(deployment)).toBe("legion-demo-controller");
  expect(tradeSession(deployment, "   ")).toBe("legion-demo-controller");
  // Neither: the trade names no session, and the fleet entry's own name stands.
  expect(tradeSession({})).toBe("");
});

test("a process that turns the person trade off is refused a device login's trade", async () => {
  // The controller sets PERSON_SIGNIN_TRADE=off so it never acts in its operator's name, and the
  // broker mints a device login's trade as the person who approved the login.
  const path = join(makeDirectory(), "legion", "device-login.json");
  await writeDeviceLoginFile(path, "http://127.0.0.1:1");
  const env = {
    [BROKER_URL_VAR]: "http://127.0.0.1:1",
    [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
    [DEVICE_LOGIN_FILE_VAR]: path,
  };
  // Control: with the person trade on, the same machine's device login makes its client.
  const made = await brokerTokenClient(env, "ses-laptop-1");
  if (made === undefined) throw new Error("the factory made no client for a device login");
  made.client.close();
  const refusal = await brokerTokenClient(
    { ...env, [PERSON_TRADE_VAR]: "off" },
    "ses-laptop-1"
  ).then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusal?.message).toContain(`${PERSON_TRADE_VAR}=off`);
  expect(refusal?.message).toContain(`${IDENTITY_VAR}=${IDENTITY_DEVICE_LOGIN}`);
});

test("device-login is an identity of its own, named with its variables", () => {
  expect(() => machineIdentity({ [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN })).toThrow(
    `${BROKER_URL_VAR} is required by ${IDENTITY_VAR}=device-login`
  );
  const named = machineIdentity({
    [IDENTITY_VAR]: IDENTITY_DEVICE_LOGIN,
    [BROKER_URL_VAR]: "https://broker.test",
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
    if (made === undefined) throw new Error("the factory made no client for a device login");
    await made.client.token();
    made.client.close();
    expect(stub.trades.at(-1)?.form.get("session")).toBe(session);
  }
  // The name's form, held apart from the function that makes it.
  expect(deviceLoginSessionName("abcdefghijklmnopqrstuvwxyz0123456789-_ABCDEFG")).toBe(
    "device-login-abcdefghijkl"
  );
});

test("a device login's read trade asks for read and its cut, proved with the login's key beside a holder key of its own", async () => {
  // The login's key signs the machine login's launcher proofs, and the broker binds no read token
  // to a key that signs them. The login's live access token is spent as it is, so only the trades
  // reach this stand-in, which records the holder proof (`DPoP-Holder`).
  const stub = stubBroker(() => ({
    body: {
      access_token: fixtureSecret("traded"),
      issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
      token_type: "DPoP",
      expires_in: 600,
    },
  }));
  stops.push(stub.stop);
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
  const cut = "2026-11-19T08:30:00.123456Z";
  const read = await brokerTokenClient(env, "ses-laptop-1", { asOf: cut });
  const plain = await brokerTokenClient(env, "ses-laptop-1");
  if (read === undefined || plain === undefined) {
    throw new Error("the factory made no client for a device login");
  }
  await read.client.token();
  await plain.client.token();
  read.client.close();
  plain.client.close();

  expect(stub.trades).toHaveLength(2);
  const [readTrade, plainTrade] = stub.trades;
  expect(readTrade?.form.get(FORM_SCOPE)).toBe(SCOPE_READ);
  expect(readTrade?.form.get(FORM_AS_OF)).toBe(cut);
  expect(readTrade?.form.has(FORM_LAUNCHER_CREDENTIAL)).toBe(false);
  // Proved with the login's key, which the broker binds a trade of its token to, and bound to a
  // holder key that is not it, which signs every proof a caller presents beside the token.
  expect(await calculateJwkThumbprint(readTrade?.proofHeader.jwk as JWK)).toBe(written.thumbprint);
  const holderJwk = readTrade?.holderHeader.jwk as JWK;
  expect(await calculateJwkThumbprint(holderJwk)).not.toBe(written.thumbprint);
  expect(read.client.key.publicJwk).toEqual(holderJwk);
  // Asked for no read, the same login sends no holder proof.
  expect(plainTrade?.holderHeader).toEqual({});
});

test("a device login takes an http broker on this machine's loopback alone", async () => {
  // The login's refresh token and access tokens cross this connection, so a broker off this
  // machine is https; a dev stack's broker on the loopback is http.
  const sent: string[] = [];
  const recording = (async (input: string | URL | Request) => {
    sent.push(String(input));
    return new Response("{}", { status: 500 });
  }) as typeof fetch;
  for (const brokerUrl of ["http://broker.test", "http://10.0.0.5:8080"]) {
    const refusal = await startDeviceLogin({
      brokerUrl,
      person: PERSON,
      host: HOST,
      fetch: recording,
    }).then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refusal?.message, brokerUrl).toContain("loopback");
    expect(refusal?.message, brokerUrl).toContain(BROKER_URL_VAR);
    expect(() => new DeviceLoginIdentity({ path: loginPath(), brokerUrl }), brokerUrl).toThrow(
      "loopback"
    );
  }
  expect(sent).toEqual([]);
  for (const brokerUrl of [
    "https://broker.test",
    "http://127.0.0.1:1",
    "http://localhost:1",
    "http://[::1]:1",
  ]) {
    expect(
      () => new DeviceLoginIdentity({ path: loginPath(), brokerUrl }),
      brokerUrl
    ).not.toThrow();
  }
});
