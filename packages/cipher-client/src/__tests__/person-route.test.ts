// packages/cipher-client/src/__tests__/person-route.test.ts
//
// The client the factory builds on a machine where a person ran `agent-secrets login`: it trades
// that person, with this machine as the actor and the sign-in's box key as the proof's key, under
// the caller's own session or the sign-in's name and never the machine's. With no sign-in it is
// the machine's own client, with every field the factory always gave it. Anything that stops the
// person's route is an error, never the machine's client in its place, except in a process that
// turns the person trade off (`PERSON_SIGNIN_TRADE=off`), which is the machine's own client
// whatever the sign-in store holds, and reads none of it.

import { afterAll, afterEach, beforeAll, expect, test } from "bun:test";
import { createPublicKey, generateKeyPairSync } from "node:crypto";
import { mkdir, writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { calculateJwkThumbprint, compactVerify, importJWK, type JWK } from "jose";
import { signedIn, stubIssuer, writeSignIn } from "../../test/sign-ins";
import {
  BROKER_URL_VAR,
  type BrokerEnvironment,
  brokerTokenClient,
  IDENTITY_VAR,
  MACHINE_AUDIENCE_VAR,
  PROJECTS_VAR,
  SESSION_VAR,
} from "../environment";
import { METADATA_HOST_VAR } from "../metadata-identity";
import {
  PERSON_TRADE_VAR,
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
  FORM_AS_OF,
  FORM_LAUNCHER_CREDENTIAL,
  FORM_LAUNCHER_CREDENTIAL_DECLINED,
  FORM_PROJECTS,
  FORM_SCOPE,
  FORM_SESSION,
  SCOPE_READ,
  SUBJECT_TOKEN_TYPE_GOOGLE,
  SUBJECT_TOKEN_TYPE_PERSON,
  TOKEN_PATH,
} from "../wire";
import { accepted, stubBroker } from "./stub";

const PERSON = "ada@example.com";
const CLIENT_ID = "example-device-client";
const CLIENT_SECRET = fixtureSecret("client");
const MACHINE_TOKEN = fixtureSecret("machine");
/** The name the machine trades under for a caller that acts as no session of its own. */
const MACHINE_SESSION = "legion-demo-controller";
/** The token the stub broker answers a read trade's nth trade with, at READ_TOKENS[n - 1]. */
const READ_TOKENS = [fixtureSecret("read1"), fixtureSecret("read2"), fixtureSecret("read3")];

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

test("a person's trade names the projects AGENT_SECRETS_PROJECTS lists, sorted, and names none when it lists none", async () => {
  const { broker, issuer } = await stubs();
  const env = await box(broker.url, issuer.url, [await signedIn(PERSON, { client_id: CLIENT_ID })]);
  await (await clientOf({ ...env, [PROJECTS_VAR]: " PUB  PRIV " }, "ses_01a9")).token();
  await (await clientOf(env, "ses_01a9")).token();
  expect(broker.trades.map((trade) => trade.form.get(FORM_PROJECTS))).toEqual(["PRIV PUB", null]);
  // A list that is no list stops the factory, naming the variable, rather than trading for fewer
  // projects than the session was started with.
  expect((await refusalOf({ ...env, [PROJECTS_VAR]: "priv" }))?.message).toContain(PROJECTS_VAR);
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

test("a person's read trade asks for scope read, sends no launcher_credential and reads an answer with no credential_id", async () => {
  // The broker answers a read trade with no launcher credential: it makes, moves and extends none.
  const broker = stubBroker((_trade, count) => ({
    body: {
      access_token: READ_TOKENS[count - 1],
      issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
      token_type: "DPoP",
      expires_in: 600,
      scope: SCOPE_READ,
    },
  }));
  const issuer = await stubIssuer({ clientId: CLIENT_ID, person: PERSON });
  stops.push(broker.stop, issuer.stop);
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const env = await box(broker.url, issuer.url, [record]);
  const cut = "2026-11-19T08:30:00.123456Z";
  const made = await brokerTokenClient(env, "ses_01a9", {});
  const cutMade = await brokerTokenClient(env, "ses_01a9", { asOf: cut });
  if (made === undefined || cutMade === undefined)
    throw new Error("the factory answered no client");

  const token = await made.client.token();
  await cutMade.client.token();

  const [read, cutRead] = broker.trades;
  expect(read?.form.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_PERSON);
  expect(read?.form.get("actor_token_type")).toBe(ACTOR_TOKEN_TYPE_MACHINE);
  expect(read?.form.get(FORM_SCOPE)).toBe(SCOPE_READ);
  expect(read?.form.has(FORM_AS_OF)).toBe(false);
  // Absent, a read trade's launcher_credential reads as declined, and a person's trade may not
  // send `false`, so the parameter is left out.
  expect(read?.form.has(FORM_LAUNCHER_CREDENTIAL)).toBe(false);
  expect(cutRead?.form.get(FORM_SCOPE)).toBe(SCOPE_READ);
  expect(cutRead?.form.get(FORM_AS_OF)).toBe(cut);
  expect(cutRead?.form.has(FORM_LAUNCHER_CREDENTIAL)).toBe(false);
  expect(token.accessToken).toBe(READ_TOKENS[0]);
  expect(token.credentialId).toBe("");
  expect(token.credentialExpiresAt).toBeNull();
  // The sign-in a refusal is remembered by is still the box key's.
  expect(made.signIn).toEqual({ person: PERSON, thumbprint: record.thumbprint });
});

test("a person's read trade signs its DPoP proof with the box key, sends a DPoP-Holder proof with a key of its own, and signs every later proof with that key", async () => {
  // The first answer asks for a nonce, which both proofs of the retried trade must carry.
  const broker = stubBroker((_trade, count) =>
    count === 1
      ? { status: 400, body: { error: "use_dpop_nonce" }, nonce: "nonce-1" }
      : { body: accepted(READ_TOKENS[count - 1], 600) }
  );
  const issuer = await stubIssuer({ clientId: CLIENT_ID, person: PERSON });
  stops.push(broker.stop, issuer.stop);
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const env = await box(broker.url, issuer.url, [record]);
  const made = await brokerTokenClient(env, "ses_01a9", {});
  if (made === undefined) throw new Error("the factory answered no client");
  const { client } = made;
  const token = await client.token();
  // A later trade of the same client, as a renewal makes.
  client.drop();
  await client.token();

  const { kty, crv, x, y } = createPublicKey(record.box_key).export({ format: "jwk" });
  const boxJwk = { kty, crv, x, y };
  const [asked, traded, renewed] = broker.trades;
  expect(broker.trades).toHaveLength(3);
  const endpoint = broker.url + TOKEN_PATH;
  for (const trade of [asked, traded, renewed]) {
    // The sign-in is proved by the box key, as every trade of it is.
    expect(trade?.proofHeader.jwk).toEqual(boxJwk);
    // The holder proof is a DPoP proof of the same request, by another P-256 key.
    expect(trade?.holderHeader).toMatchObject({ typ: "dpop+jwt", alg: "ES256" });
    const holderJwk = trade?.holderHeader.jwk as JWK;
    expect(Object.keys(holderJwk).sort()).toEqual(["crv", "kty", "x", "y"]);
    expect(holderJwk).not.toEqual(boxJwk);
    expect(trade?.holderClaims).toMatchObject({ htm: "POST", htu: endpoint });
    expect(typeof trade?.holderClaims.jti).toBe("string");
    expect(trade?.holderClaims.jti).not.toBe(trade?.proofClaims.jti);
    expect(typeof trade?.holderClaims.iat).toBe("number");
  }
  expect(asked?.holderClaims.nonce).toBeUndefined();
  expect(traded?.proofClaims.nonce).toBe("nonce-1");
  expect(traded?.holderClaims.nonce).toBe("nonce-1");
  // One holder key for the client's life, so every token it trades is bound to the same key.
  const holderJwk = traded?.holderHeader.jwk as JWK;
  expect(renewed?.holderHeader.jwk).toEqual(holderJwk);

  // The key a caller signs Dispatch's proofs with is the holder key, and never the box key.
  expect(client.key.publicJwk).toEqual(holderJwk);
  const proof = await client.key.proof({
    method: "GET",
    url: "http://dispatch.test/api/v1/whoami",
    accessToken: token.accessToken,
  });
  const header = JSON.parse(Buffer.from(proof.split(".")[0] ?? "", "base64url").toString("utf8"));
  expect(header.jwk).toEqual(holderJwk);
  await compactVerify(proof, await importJWK(holderJwk, "ES256"));
  expect(
    await compactVerify(proof, await importJWK(boxJwk as JWK, "ES256")).then(
      () => "verified",
      () => "refused"
    )
  ).toBe("refused");
});

test("a trade that asks for no scope sends no scope, no cut and no holder proof", async () => {
  const { broker, issuer } = await stubs();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const person = await clientOf(await box(broker.url, issuer.url, [record]), "ses_01a9");
  const machine = await clientOf(await box(broker.url, issuer.url, []), "ses_01a9");
  await person.token();
  await machine.token();
  for (const trade of broker.trades) {
    expect(trade.form.has(FORM_SCOPE)).toBe(false);
    expect(trade.form.has(FORM_AS_OF)).toBe(false);
    expect(trade.holderHeader).toEqual({});
  }
  // A machine's read trade proves a key this process made, which holds no launcher credential,
  // so it binds that key and sends no holder proof.
  const made = await brokerTokenClient(await box(broker.url, issuer.url, []), "ses_01a9", {});
  await made?.client.token();
  const read = broker.trades[2];
  expect(read?.form.get(FORM_SCOPE)).toBe(SCOPE_READ);
  expect(read?.form.get(FORM_LAUNCHER_CREDENTIAL)).toBe(FORM_LAUNCHER_CREDENTIAL_DECLINED);
  expect(read?.holderHeader).toEqual({});
  expect(read?.proofHeader.jwk).toEqual(made?.client.key.publicJwk);
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

test("with PERSON_SIGNIN_TRADE=off, a machine with a sign-in trades itself: its own subject, no actor, tradeSession and a key of its own", async () => {
  const { broker, issuer } = await stubs();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const env = { ...(await box(broker.url, issuer.url, [record])), [PERSON_TRADE_VAR]: "off" };
  const made = await brokerTokenClient(env);
  const acting = await brokerTokenClient(env, " ses_01a9 ");
  // The answer names no sign-in, as on a machine where nobody signed in.
  expect(made?.signIn).toBeUndefined();
  expect(acting?.signIn).toBeUndefined();
  expect(made?.client.describe()).toContain("this machine's Google identity token");
  await made?.client.token();
  await acting?.client.token();
  const [machine, own] = broker.trades;
  expect(machine?.form.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_GOOGLE);
  expect(machine?.form.get("subject_token")).toBe(MACHINE_TOKEN);
  expect(machine?.form.has("actor_token")).toBe(false);
  expect(machine?.form.has("actor_token_type")).toBe(false);
  expect(machine?.form.get(FORM_LAUNCHER_CREDENTIAL)).toBe(FORM_LAUNCHER_CREDENTIAL_DECLINED);
  // Under the name the machine trades under, never the sign-in's.
  expect(machine?.form.get(FORM_SESSION)).toBe(MACHINE_SESSION);
  expect(own?.form.get(FORM_SESSION)).toBe("ses_01a9");
  expect(own?.form.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_GOOGLE);
  // The proof is signed with a key this process made, never the sign-in's box key.
  const jwk = machine?.proofHeader.jwk;
  expect(jwk).toEqual(made?.client.key.publicJwk);
  expect(await calculateJwkThumbprint(jwk as JWK)).not.toBe(record.thumbprint);
  // Nobody's sign-in was renewed.
  expect(issuer.requests).toHaveLength(0);
});

test("with PERSON_SIGNIN_TRADE=off, a store that refuses the person's trade refuses nothing: two sign-ins and nobody named, or a record that cannot be read", async () => {
  const { broker, issuer } = await stubs();
  const two = await box(broker.url, issuer.url, [
    await signedIn(PERSON),
    await signedIn("grace@example.com"),
  ]);
  const broken = await box(broker.url, issuer.url, []);
  const signins = broken[SIGNIN_DIR_VAR] as string;
  await mkdir(signins, { recursive: true, mode: 0o700 });
  await writeFile(join(signins, "broken.json"), "{", { mode: 0o600 });
  for (const env of [two, broken]) {
    // With the person trade on, each store is refused.
    expect(await refusalOf(env)).not.toBeNull();
    const made = await brokerTokenClient({ ...env, [PERSON_TRADE_VAR]: "off" });
    expect(made?.signIn).toBeUndefined();
    await made?.client.token();
  }
  expect(
    broker.trades.map((trade) => [
      trade.form.get("subject_token_type"),
      trade.form.has("actor_token"),
      trade.form.get(FORM_SESSION),
    ])
  ).toEqual([
    [SUBJECT_TOKEN_TYPE_GOOGLE, false, MACHINE_SESSION],
    [SUBJECT_TOKEN_TYPE_GOOGLE, false, MACHINE_SESSION],
  ]);
  expect(issuer.requests).toHaveLength(0);
});

test("a PERSON_SIGNIN_TRADE that is neither off nor blank is refused naming the variable and its value, and nothing trades", async () => {
  const { broker, issuer } = await stubs();
  const env = await box(broker.url, issuer.url, [await signedIn(PERSON, { client_id: CLIENT_ID })]);
  expect((await refusalOf({ ...env, [PERSON_TRADE_VAR]: "0" }))?.message).toBe(
    `${PERSON_TRADE_VAR} is "0" and takes off or nothing: set it to off where this process's own credentials trade this machine and never a person signed in on it, and leave it unset where they trade the person signed in here`
  );
  for (const written of ["on", " OFF ", "false"]) {
    const refusal = await refusalOf({ ...env, [PERSON_TRADE_VAR]: written });
    expect(refusal?.message, written).toStartWith(
      `${PERSON_TRADE_VAR} is "${written.trim()}" and takes off or nothing:`
    );
  }
  // Refused as well where nobody signed in: the setting is read before the store.
  const nobody = await box(broker.url, issuer.url, []);
  expect((await refusalOf({ ...nobody, [PERSON_TRADE_VAR]: "on" }))?.message).toStartWith(
    `${PERSON_TRADE_VAR} is "on"`
  );
  expect(broker.trades).toHaveLength(0);
  expect(issuer.requests).toHaveLength(0);
});

test("a blank PERSON_SIGNIN_TRADE is unset: the person signed in here is traded as before", async () => {
  const { broker, issuer } = await stubs();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const env = await box(broker.url, issuer.url, [record]);
  for (const blank of ["", "  "]) {
    const made = await brokerTokenClient({ ...env, [PERSON_TRADE_VAR]: blank });
    expect(made?.signIn).toEqual({ person: PERSON, thumbprint: record.thumbprint });
    await made?.client.token();
  }
  expect(
    broker.trades.map((trade) => [
      trade.form.get("subject_token_type"),
      trade.form.get("actor_token_type"),
      trade.form.get(FORM_SESSION),
    ])
  ).toEqual([
    [SUBJECT_TOKEN_TYPE_PERSON, ACTOR_TOKEN_TYPE_MACHINE, signInSessionName(record)],
    [SUBJECT_TOKEN_TYPE_PERSON, ACTOR_TOKEN_TYPE_MACHINE, signInSessionName(record)],
  ]);
});
