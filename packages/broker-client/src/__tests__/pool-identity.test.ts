// packages/broker-client/src/__tests__/pool-identity.test.ts
//
// The workload identity pool's whole path, against stand-ins for Google's security token service
// and IAM Credentials, in the shape of the Go client's own test
// (`packages/envoy/internal/brokerclient/pool_test.go`): the credential this machine already holds
// is traded at the security token service for a federated access token, that token is spent on
// one generateIdToken with includeEmail — the broker looks a machine up by the token's email
// claim — and the identity token that mint answers is what the broker is shown. A person's trade
// on such a machine shows it as the actor, and a pool that refuses stops the trade before the
// broker is asked anything.

import { afterEach, expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { importDpopKey } from "../dpop";
import {
  IDENTITY_VAR,
  MACHINE_AUDIENCE_VAR,
  MACHINE_TOKEN_FILE_VAR,
  machineIdentity,
  POOL_ACCOUNT_VAR,
  POOL_AUDIENCE_VAR,
} from "../environment";
import { personIdentity } from "../person-identity";
import { poolIdentity } from "../pool-identity";
import { projectedToken } from "../projected-token";
import { BrokerTokenClient } from "../token-client";
import {
  ACTOR_TOKEN_TYPE_MACHINE,
  SUBJECT_TOKEN_TYPE_GOOGLE,
  SUBJECT_TOKEN_TYPE_PERSON,
} from "../wire";
import { signedIn } from "./signin-records";
import { accepted, stubBroker, stubIssuer } from "./stub";

const EXTERNAL_TOKEN = "the.credential.this.machine.holds";
const FEDERATED_TOKEN = "federated-access-token";
const MINTED_TOKEN = "google.identity.token.minted.for.the.pool";
const POOL_AUDIENCE =
  "//iam.googleapis.com/projects/000000000000/locations/global/workloadIdentityPools/example-pool/providers/example-provider";
const SERVICE_ACCOUNT = "example-fleet@example-project.iam.gserviceaccount.com";
const CLIENT_ID = "example-machine-client-id";
const PERSON = "ada@example.com";
const SIGNIN_CLIENT_ID = "example-device-client";

const makeDirectory = temporaryDirectories("pool-identity");

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** One request a stand-in was sent, as it saw it. */
interface Posted {
  readonly method: string;
  readonly path: string;
  readonly contentType: string | null;
  readonly authorization: string | null;
  readonly body: unknown;
}

/** What a stand-in answers one request with. A string body is sent as it is written. */
interface Answer {
  readonly status?: number;
  readonly body: Record<string, unknown> | string;
}

/** A stand-in for one of Google's two services, answering `answer` and keeping every request. */
function stubService(answer: () => Answer): { readonly url: string; readonly posted: Posted[] } {
  const posted: Posted[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      posted.push({
        method: request.method,
        path: new URL(request.url).pathname,
        contentType: request.headers.get("Content-Type"),
        authorization: request.headers.get("Authorization"),
        body: await request.json(),
      });
      const given = answer();
      const status = given.status ?? 200;
      return typeof given.body === "string"
        ? new Response(given.body, { status })
        : Response.json(given.body, { status });
    },
  });
  stops.push(() => server.stop(true));
  return { url: `http://127.0.0.1:${server.port}`, posted };
}

/** What Google's security token service answers: {@link FEDERATED_TOKEN}. */
const federating = (): Answer => ({
  body: {
    access_token: FEDERATED_TOKEN,
    issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
    token_type: "Bearer",
    expires_in: 3600,
  },
});

/** What IAM Credentials' generateIdToken answers: {@link MINTED_TOKEN}. */
const minting = (): Answer => ({ body: { token: MINTED_TOKEN } });

/** The projected token file this machine holds, holding {@link EXTERNAL_TOKEN}. */
async function externalCredential(): Promise<string> {
  const path = join(makeDirectory(), "token");
  await writeFile(path, `${EXTERNAL_TOKEN}\n`, { mode: 0o600 });
  return path;
}

/** The pool source over the two stand-ins, federating this machine's projected token. */
async function poolAt(sts: string, iam: string) {
  return poolIdentity({
    subject: projectedToken(await externalCredential()),
    audience: POOL_AUDIENCE,
    serviceAccount: SERVICE_ACCOUNT,
    tokenAudience: CLIENT_ID,
    securityTokenServiceUrl: `${sts}/v1/token`,
    iamCredentialsUrl: iam,
  });
}

/** The error `run` rejects with, or null where it resolves. */
async function refusalOf(run: () => Promise<unknown>): Promise<Error | null> {
  return await run().then(
    () => null,
    (error: unknown) => error as Error
  );
}

test("the pool posts Go's JSON body to the security token service, the projected token as a jwt", async () => {
  const sts = stubService(federating);
  const iam = stubService(minting);
  await (await poolAt(sts.url, iam.url)).read();
  expect(sts.posted).toEqual([
    {
      method: "POST",
      path: "/v1/token",
      contentType: "application/json",
      // The security token service is asked with no credential but the one in the body.
      authorization: null,
      body: {
        audience: POOL_AUDIENCE,
        grantType: "urn:ietf:params:oauth:grant-type:token-exchange",
        requestedTokenType: "urn:ietf:params:oauth:token-type:access_token",
        scope: "https://www.googleapis.com/auth/cloud-platform",
        // A Kubernetes projected token is a JWT to the pool's OIDC provider, whatever name the
        // broker's own exchange knows it by.
        subjectTokenType: "urn:ietf:params:oauth:token-type:jwt",
        subjectToken: EXTERNAL_TOKEN,
      },
    },
  ]);
});

test("generateIdToken is called on the pool's account, with the federated token as bearer and includeEmail", async () => {
  const sts = stubService(federating);
  const iam = stubService(minting);
  await (await poolAt(sts.url, iam.url)).read();
  expect(iam.posted).toEqual([
    {
      method: "POST",
      // The managed identity the pool lets this credential act as, its address as Go writes it.
      path: `/v1/projects/-/serviceAccounts/${SERVICE_ACCOUNT}:generateIdToken`,
      contentType: "application/json",
      authorization: `Bearer ${FEDERATED_TOKEN}`,
      // includeEmail, since the broker looks a machine up by the token's email claim; the
      // audience is the client id the broker checks.
      body: { audience: CLIENT_ID, includeEmail: true },
    },
  ]);
});

test("the pool answers the token generateIdToken minted, as a machine's Google identity token", async () => {
  const sts = stubService(federating);
  const iam = stubService(minting);
  expect(await (await poolAt(sts.url, iam.url)).read()).toEqual({
    token: MINTED_TOKEN,
    tokenType: SUBJECT_TOKEN_TYPE_GOOGLE,
  });
});

test("a security token service that refuses is an error naming it, its status and its reason", async () => {
  const reason =
    '{"error":"invalid_grant","error_description":"the subject token is not this provider\'s"}';
  const sts = stubService(() => ({ status: 400, body: reason }));
  const iam = stubService(minting);
  const refusal = await refusalOf(async () => (await poolAt(sts.url, iam.url)).read());
  expect(refusal?.message).toBe(
    `trade this machine's credential at the security token service ${sts.url}/v1/token: HTTP 400: ${reason}`
  );
  expect(refusal?.message).not.toContain(EXTERNAL_TOKEN);
  // Nothing is minted with no federated token.
  expect(iam.posted).toHaveLength(0);
});

test("a security token service that answers no access_token is an error naming it", async () => {
  const sts = stubService(() => ({ body: { token_type: "Bearer", expires_in: 3600 } }));
  const iam = stubService(minting);
  const refusal = await refusalOf(async () => (await poolAt(sts.url, iam.url)).read());
  expect(refusal?.message).toBe(
    `the security token service ${sts.url}/v1/token answered no access_token`
  );
  expect(iam.posted).toHaveLength(0);
});

test("generateIdToken refusing is an error naming it, its status and its reason", async () => {
  const reason =
    '{"error":{"code":403,"message":"Permission iam.serviceAccounts.getOpenIdToken denied"}}';
  const sts = stubService(federating);
  const iam = stubService(() => ({ status: 403, body: reason }));
  const refusal = await refusalOf(async () => (await poolAt(sts.url, iam.url)).read());
  expect(refusal?.message).toBe(
    `mint this machine's identity token at ${iam.url}/v1/projects/-/serviceAccounts/${SERVICE_ACCOUNT}:generateIdToken: HTTP 403: ${reason}`
  );
  expect(refusal?.message).not.toContain(FEDERATED_TOKEN);
});

test("generateIdToken answering no token is an error naming it", async () => {
  const sts = stubService(federating);
  const iam = stubService(() => ({ body: {} }));
  const refusal = await refusalOf(async () => (await poolAt(sts.url, iam.url)).read());
  expect(refusal?.message).toBe(
    `generateIdToken at ${iam.url}/v1/projects/-/serviceAccounts/${SERVICE_ACCOUNT}:generateIdToken answered no token`
  );
});

test("an answer that is not JSON is named without its text, which could be a token", async () => {
  const sts = stubService(() => ({ body: FEDERATED_TOKEN }));
  const iam = stubService(minting);
  const refusal = await refusalOf(async () => (await poolAt(sts.url, iam.url)).read());
  expect(refusal?.message).toBe(
    `trade this machine's credential at the security token service ${sts.url}/v1/token: HTTP 200: the answer is not JSON`
  );
  expect(refusal?.message).not.toContain(FEDERATED_TOKEN);
});

test("an external credential that cannot be read stops the pool before either service is asked", async () => {
  const sts = stubService(federating);
  const iam = stubService(minting);
  const missing = join(makeDirectory(), "absent");
  const source = poolIdentity({
    subject: projectedToken(missing),
    audience: POOL_AUDIENCE,
    serviceAccount: SERVICE_ACCOUNT,
    tokenAudience: CLIENT_ID,
    securityTokenServiceUrl: `${sts.url}/v1/token`,
    iamCredentialsUrl: iam.url,
  });
  const refusal = await refusalOf(() => source.read());
  expect(refusal?.message).toStartWith(
    `read the credential the workload identity pool federates: read the projected service-account token ${missing}: `
  );
  expect(sts.posted).toHaveLength(0);
  expect(iam.posted).toHaveLength(0);
});

/** A pool machine's four variables, as a deployment writes them. */
const POOL_MACHINE = {
  [IDENTITY_VAR]: "pool",
  [MACHINE_TOKEN_FILE_VAR]: "/var/run/secrets/pool/token",
  [MACHINE_AUDIENCE_VAR]: CLIENT_ID,
  [POOL_AUDIENCE_VAR]: POOL_AUDIENCE,
  [POOL_ACCOUNT_VAR]: SERVICE_ACCOUNT,
};

test("AGENT_SECRETS_MACHINE_IDENTITY=pool with its four variables is the pool source", () => {
  // The machine audience is the minted token's own, the pool audience the provider the projected
  // token is presented to, and the account the one generateIdToken mints as: Go's mapping.
  expect(machineIdentity(POOL_MACHINE)?.describe()).toBe(
    `a Google identity token for ${CLIENT_ID} minted through the workload identity pool ${POOL_AUDIENCE} as ${SERVICE_ACCOUNT}`
  );
});

test("AGENT_SECRETS_MACHINE_IDENTITY=pool with any of its four variables missing is refused naming it", () => {
  for (const name of [
    MACHINE_TOKEN_FILE_VAR,
    MACHINE_AUDIENCE_VAR,
    POOL_AUDIENCE_VAR,
    POOL_ACCOUNT_VAR,
  ]) {
    expect(() => machineIdentity({ ...POOL_MACHINE, [name]: " " })).toThrow(
      `${name} is required by ${IDENTITY_VAR}=pool`
    );
  }
});

/** The person's trade on a pool machine: their sign-in renewed at a stub issuer is the subject,
 *  the pool over the two stand-ins is the actor, and the sign-in's box key signs the proof. */
async function personOnPoolMachine(sts: string, iam: string, brokerUrl: string) {
  const issuer = await stubIssuer({ clientId: SIGNIN_CLIENT_ID, person: PERSON });
  stops.push(issuer.stop);
  const record = await signedIn(PERSON, { client_id: SIGNIN_CLIENT_ID });
  const client = new BrokerTokenClient({
    brokerUrl,
    key: await importDpopKey(record.box_key, `${PERSON}'s box key`),
    subject: personIdentity(record, {
      issuer: issuer.url,
      clientId: SIGNIN_CLIENT_ID,
      clientSecret: "example-device-client-not-a-secret",
    }),
    actor: await poolAt(sts, iam),
    session: "ses_01a9",
  });
  return { client, issuer };
}

test("a person's trade on a pool machine shows the broker the minted token as the machine actor", async () => {
  const sts = stubService(federating);
  const iam = stubService(minting);
  const broker = stubBroker(() => ({ body: accepted("dispatch-token-for-ada", 600) }));
  stops.push(broker.stop);
  const { client, issuer } = await personOnPoolMachine(sts.url, iam.url, broker.url);
  expect((await client.token()).accessToken).toBe("dispatch-token-for-ada");
  expect(broker.trades).toHaveLength(1);
  const form = broker.trades[0]?.form;
  expect(form?.get("subject_token")).toBe(issuer.idTokens[0] as string);
  expect(form?.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_PERSON);
  expect(form?.get("actor_token")).toBe(MINTED_TOKEN);
  expect(form?.get("actor_token_type")).toBe(ACTOR_TOKEN_TYPE_MACHINE);
});

test("with the security token service refusing, a person's trade fails naming it and the broker is asked nothing", async () => {
  const sts = stubService(() => ({ status: 400, body: '{"error":"invalid_grant"}' }));
  const iam = stubService(minting);
  const broker = stubBroker(() => ({ body: accepted("never", 600) }));
  stops.push(broker.stop);
  const { client } = await personOnPoolMachine(sts.url, iam.url, broker.url);
  const refusal = await refusalOf(() => client.token());
  expect(refusal?.message).toContain(
    `the security token service ${sts.url}/v1/token: HTTP 400: {"error":"invalid_grant"}`
  );
  expect(broker.trades).toHaveLength(0);
  expect(iam.posted).toHaveLength(0);
});
