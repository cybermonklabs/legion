// packages/broker-client/src/__tests__/person-route.test.ts
//
// The client the factory builds on a machine where a person ran `agent-secrets login`: it trades
// that person, with this machine as the actor and the sign-in's box key as the proof's key, under
// the caller's own session or the sign-in's name and never the machine's. With no sign-in it is
// the machine's own client, with every field the factory always gave it. Anything that stops the
// person's route is an error, never the machine's client in its place.

import { afterAll, afterEach, beforeAll, expect, test } from "bun:test";
import { createPublicKey, generateKeyPairSync } from "node:crypto";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { calculateJwkThumbprint, type JWK } from "jose";
import { signedIn, stubIssuer, writeSignIn } from "../../test/sign-ins";
import {
  BROKER_URL_VAR,
  type BrokerEnvironment,
  brokerTokenClient,
  IDENTITY_VAR,
  MACHINE_AUDIENCE_VAR,
  SESSION_VAR,
} from "../environment";
import { METADATA_HOST_VAR } from "../metadata-identity";
import {
  PERSON_VAR,
  SIGNIN_CLIENT_ID_VAR,
  SIGNIN_CLIENT_SECRET_FILE_VAR,
  SIGNIN_COMMAND,
  SIGNIN_DIR_VAR,
  SIGNIN_ISSUER_VAR,
  type SignInRecord,
  signInSessionName,
} from "../signins";
import type { BrokerTokenClient } from "../token-client";
import {
  ACTOR_TOKEN_TYPE_MACHINE,
  FORM_LAUNCHER_CREDENTIAL,
  FORM_LAUNCHER_CREDENTIAL_DECLINED,
  FORM_SESSION,
  SUBJECT_TOKEN_TYPE_GOOGLE,
  SUBJECT_TOKEN_TYPE_PERSON,
} from "../wire";
import { accepted, stubBroker } from "./stub";

const PERSON = "ada@example.com";
const CLIENT_ID = "example-device-client";
const CLIENT_SECRET = "example-device-client-not-a-secret";
const MACHINE_TOKEN = "machine.identity.token";
/** The name the machine trades under for a caller that acts as no session of its own. */
const MACHINE_SESSION = "legion-demo-controller";

const makeDirectory = temporaryDirectories("person-route");

/**
 * The metadata server this file's machine asks for its own Google identity token. The metadata
 * identity reads `GCE_METADATA_HOST` from the process's environment, so the variable is set for
 * this file and put back after it.
 */
let stopMetadata = () => {};
let previousMetadataHost: string | undefined;
beforeAll(() => {
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => new Response(`${MACHINE_TOKEN}\n`),
  });
  stopMetadata = () => server.stop(true);
  previousMetadataHost = process.env[METADATA_HOST_VAR];
  process.env[METADATA_HOST_VAR] = `127.0.0.1:${server.port}`;
});
afterAll(() => {
  stopMetadata();
  if (previousMetadataHost === undefined) delete process.env[METADATA_HOST_VAR];
  else process.env[METADATA_HOST_VAR] = previousMetadataHost;
});

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A stub broker that accepts every trade, and a stub issuer for {@link PERSON}, both stopped
 *  after the test. */
async function stubs() {
  const broker = stubBroker((_trade, count) => ({ body: accepted(`broker-token-${count}`, 600) }));
  const issuer = await stubIssuer({ clientId: CLIENT_ID, person: PERSON });
  stops.push(broker.stop, issuer.stop);
  return { broker, issuer };
}

/**
 * The environment of a Compute Engine box at `brokerUrl` whose sign-in directory holds
 * `records`, with the sign-in client at `issuer` configured and `AGENT_SECRETS_SESSION` set, as a
 * daemon's environment sets it.
 */
async function box(
  brokerUrl: string,
  issuer: string,
  records: readonly SignInRecord[]
): Promise<BrokerEnvironment> {
  const dir = makeDirectory();
  const signins = join(dir, "signins");
  for (const record of records) await writeSignIn(signins, record);
  const secret = join(dir, "client-secret");
  await writeFile(secret, `${CLIENT_SECRET}\n`, { mode: 0o600 });
  return {
    [BROKER_URL_VAR]: brokerUrl,
    [IDENTITY_VAR]: "metadata",
    [MACHINE_AUDIENCE_VAR]: "example-machine-client-id",
    [SESSION_VAR]: MACHINE_SESSION,
    [SIGNIN_DIR_VAR]: signins,
    [SIGNIN_ISSUER_VAR]: issuer,
    [SIGNIN_CLIENT_ID_VAR]: CLIENT_ID,
    [SIGNIN_CLIENT_SECRET_FILE_VAR]: secret,
  };
}

/** The client the factory answers, which these tests expect it to answer. */
async function clientOf(env: BrokerEnvironment, own?: string): Promise<BrokerTokenClient> {
  const made = await brokerTokenClient(env, own);
  if (made === undefined) throw new Error("the factory answered no client");
  return made.client;
}

/** The error the factory rejects with, or null where it answers a client. */
async function refusalOf(env: BrokerEnvironment): Promise<Error | null> {
  return await brokerTokenClient(env).then(
    () => null,
    (error: unknown) => error as Error
  );
}

test("with a sign-in, the trade is the person's: their subject, the machine's actor, no launcher_credential, the caller's session", async () => {
  const { broker, issuer } = await stubs();
  const env = await box(broker.url, issuer.url, [await signedIn(PERSON, { client_id: CLIENT_ID })]);
  const client = await clientOf(env, " ses_01a9 ");
  expect(client.describe()).toBe(`${PERSON}'s sign-in on this machine`);
  expect((await client.token()).accessToken).toBe("broker-token-1");
  expect(broker.trades).toHaveLength(1);
  const form = broker.trades[0]?.form;
  expect(form?.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_PERSON);
  expect(form?.get("subject_token")).toBe(issuer.idTokens[0] as string);
  expect(form?.get("actor_token_type")).toBe(ACTOR_TOKEN_TYPE_MACHINE);
  expect(form?.get("actor_token")).toBe(MACHINE_TOKEN);
  // The broker refuses a person's trade that declines, so the parameter is left out.
  expect(form?.has(FORM_LAUNCHER_CREDENTIAL)).toBe(false);
  expect(form?.get(FORM_SESSION)).toBe("ses_01a9");
});

test("with no session of its own, the person trades under the sign-in's name, never AGENT_SECRETS_SESSION", async () => {
  const { broker, issuer } = await stubs();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const env = await box(broker.url, issuer.url, [record]);
  await (await clientOf(env)).token();
  await (await clientOf(env, "   ")).token();
  // The machine trades under AGENT_SECRETS_SESSION, and the broker holds a name for one
  // identity, so the person asking under it would be refused.
  expect(broker.trades.map((trade) => trade.form.get(FORM_SESSION))).toEqual([
    signInSessionName(record),
    signInSessionName(record),
  ]);
  expect(signInSessionName(record)).toStartWith(`signin-${PERSON}-`);
});

test("the person's proof carries the box key's public JWK, the key the broker bound the sign-in to", async () => {
  const { broker, issuer } = await stubs();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const env = await box(broker.url, issuer.url, [record]);
  const client = await clientOf(env, "ses_01a9");
  await client.token();
  const { kty, crv, x, y } = createPublicKey(record.box_key).export({ format: "jwk" });
  const jwk = broker.trades[0]?.proofHeader.jwk;
  expect(jwk).toEqual({ kty, crv, x, y });
  expect(client.key.publicJwk).toEqual({ kty, crv, x, y });
  // The thumbprint `agent-secrets login` recorded, which a revoke of this sign-in refuses.
  expect(await calculateJwkThumbprint(jwk as JWK)).toBe(record.thumbprint);
});

test("with no sign-in, the client is the machine's own: launcher_credential=false, tradeSession and a fresh key", async () => {
  const { broker, issuer } = await stubs();
  const env = await box(broker.url, issuer.url, []);
  const first = await clientOf(env);
  const second = await clientOf(env);
  const acting = await clientOf(env, "ses_01a9");
  await first.token();
  await acting.token();
  expect(first.describe()).toContain("this machine's Google identity token");
  const [machine, own] = broker.trades;
  expect(machine?.form.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_GOOGLE);
  expect(machine?.form.get("subject_token")).toBe(MACHINE_TOKEN);
  expect(machine?.form.has("actor_token")).toBe(false);
  expect(machine?.form.get(FORM_LAUNCHER_CREDENTIAL)).toBe(FORM_LAUNCHER_CREDENTIAL_DECLINED);
  expect(machine?.form.get(FORM_SESSION)).toBe(MACHINE_SESSION);
  expect(own?.form.get(FORM_SESSION)).toBe("ses_01a9");
  expect(own?.form.get(FORM_LAUNCHER_CREDENTIAL)).toBe(FORM_LAUNCHER_CREDENTIAL_DECLINED);
  // A key made with each client, whose private half exists nowhere outside its signer.
  expect(machine?.proofHeader.jwk).toEqual(first.key.publicJwk);
  expect(first.key.publicJwk).not.toEqual(second.key.publicJwk);
  expect(Object.hasOwn(first.key.publicJwk, "d")).toBe(false);
  // Nobody signed in, so no issuer was asked for anyone.
  expect(issuer.requests).toHaveLength(0);
});

test("a sign-in with no sign-in client configured is refused naming both variables and the person, and nothing trades", async () => {
  const { broker, issuer } = await stubs();
  const env = await box(broker.url, issuer.url, [await signedIn(PERSON)]);
  const refusal = await refusalOf({
    ...env,
    [SIGNIN_CLIENT_ID_VAR]: undefined,
    [SIGNIN_CLIENT_SECRET_FILE_VAR]: undefined,
  });
  expect(refusal?.message).toBe(
    `${PERSON} is signed in on this machine, and this machine configures no sign-in client to renew that sign-in with: set ${SIGNIN_CLIENT_ID_VAR} and ${SIGNIN_CLIENT_SECRET_FILE_VAR}`
  );
  // A gap in the deployment, which no sign-in cures.
  expect(refusal?.message).not.toContain(SIGNIN_COMMAND);
  expect(broker.trades).toHaveLength(0);
});

test("a sign-in client configured by half is refused, and nothing trades", async () => {
  const { broker, issuer } = await stubs();
  const env = await box(broker.url, issuer.url, [await signedIn(PERSON)]);
  const refusal = await refusalOf({ ...env, [SIGNIN_CLIENT_SECRET_FILE_VAR]: undefined });
  expect(refusal?.message).toContain(
    `a person's sign-in needs both ${SIGNIN_CLIENT_ID_VAR} and ${SIGNIN_CLIENT_SECRET_FILE_VAR}`
  );
  expect(broker.trades).toHaveLength(0);
});

test("two sign-ins with nobody named are refused naming AGENT_SECRETS_PERSON, and nothing trades", async () => {
  const { broker, issuer } = await stubs();
  const env = await box(broker.url, issuer.url, [
    await signedIn(PERSON),
    await signedIn("grace@example.com"),
  ]);
  const refusal = await refusalOf(env);
  expect(refusal?.message).toBe(
    `2 people are signed in on this machine (ada@example.com, grace@example.com), so this session must say which one it acts for: set ${PERSON_VAR} to one of them`
  );
  expect(broker.trades).toHaveLength(0);
  // Named, the same machine trades that person.
  await (await clientOf({ ...env, [PERSON_VAR]: PERSON })).token();
  expect(broker.trades[0]?.form.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_PERSON);
});

test("a box key that does not import is refused naming whose key it is, and nothing trades", async () => {
  const { broker, issuer } = await stubs();
  const { privateKey } = generateKeyPairSync("ec", { namedCurve: "P-384" });
  const record = await signedIn(PERSON, {
    box_key: privateKey.export({ format: "pem", type: "pkcs8" }).toString(),
  });
  const refusal = await refusalOf(await box(broker.url, issuer.url, [record]));
  expect(refusal?.message).toStartWith(`${PERSON}'s box key is not a P-256 key`);
  expect(broker.trades).toHaveLength(0);
});

test("a sign-in made with another client than the configured one is refused, and nothing trades", async () => {
  const { broker, issuer } = await stubs();
  const record = await signedIn(PERSON, { client_id: "another-device-client" });
  const refusal = await refusalOf(await box(broker.url, issuer.url, [record]));
  expect(refusal?.message).toContain(`run: ${SIGNIN_COMMAND}`);
  expect(refusal?.message).toStartWith(`${PERSON} signed in with another client`);
  expect(broker.trades).toHaveLength(0);
  expect(issuer.requests).toHaveLength(0);
});

test("a sign-in record that cannot be read is refused rather than passed over, and nothing trades", async () => {
  const { broker, issuer } = await stubs();
  const env = await box(broker.url, issuer.url, []);
  const signins = env[SIGNIN_DIR_VAR] as string;
  await mkdir(signins, { recursive: true, mode: 0o700 });
  await writeFile(join(signins, "broken.json"), "{", { mode: 0o600 });
  const refusal = await refusalOf(env);
  expect(refusal?.message).toContain(join(signins, "broken.json"));
  expect(refusal?.message).toContain(SIGNIN_COMMAND);
  expect(broker.trades).toHaveLength(0);
});
