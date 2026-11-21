// packages/cipher-client/src/__tests__/pool-identity.test.ts
//
// The workload identity pool's whole path, against stand-ins for Google's security token service
// and IAM Credentials, in the shape of the Go client's own test
// (`packages/envoy/internal/cipherclient/pool_test.go`): the credential this machine already holds
// is traded at the security token service for a federated access token, that token is spent on
// one generateIdToken with includeEmail — the broker looks a machine up by the token's email
// claim — and the identity token that mint answers is what the broker is shown. A person's trade
// on such a machine shows it as the actor, and a pool that refuses stops the trade before the
// broker is asked anything.

import { afterEach, expect, test } from "bun:test";
import { randomBytes } from "node:crypto";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import { signedIn, stubIssuer } from "../../test/sign-ins";
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
import { TradeRetryingError } from "../token-errors";
import {
  ACTOR_TOKEN_TYPE_MACHINE,
  SUBJECT_TOKEN_TYPE_GOOGLE,
  SUBJECT_TOKEN_TYPE_PERSON,
} from "../wire";
import { accepted, refusalOf, stubBroker } from "./stub";

const EXTERNAL_TOKEN = fixtureSecret("external");
const FEDERATED_TOKEN = fixtureSecret("fed");
const MINTED_TOKEN = fixtureSecret("minted");
const POOL_AUDIENCE =
  "//iam.googleapis.com/projects/000000000000/locations/global/workloadIdentityPools/example-pool/providers/example-provider";
const SERVICE_ACCOUNT = "example-fleet@example-project.iam.gserviceaccount.com";
const CLIENT_ID = "example-machine-client-id";
const PERSON = "ada@example.com";
const SIGNIN_CLIENT_ID = "example-device-client";
const SIGNIN_CLIENT_SECRET = fixtureSecret("client");

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

/** The pool source over the two stand-ins, federating this machine's projected token, its calls
 *  made through `fetchImpl` where one is given. */
async function poolAt(sts: string, iam: string, fetchImpl?: typeof fetch) {
  return poolIdentity({
    subject: projectedToken(await externalCredential(), "AGENT_SECRETS_MACHINE_TOKEN_FILE"),
    audience: POOL_AUDIENCE,
    serviceAccount: SERVICE_ACCOUNT,
    tokenAudience: CLIENT_ID,
    securityTokenServiceUrl: `${sts}/v1/token`,
    iamCredentialsUrl: iam,
    fetch: fetchImpl,
  });
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

// Google's answer is named by its status and its error code alone, never its body: a service, or
// anything at its URL, can echo the token it was sent there (#4140, P-f).
test("a security token service that refuses is an error naming it, its status and its code", async () => {
  const reason = JSON.stringify({
    error: "invalid_grant",
    error_description: `the subject token ${EXTERNAL_TOKEN} is not this provider's`,
  });
  const sts = stubService(() => ({ status: 400, body: reason }));
  const iam = stubService(minting);
  const refusal = await refusalOf(async () => (await poolAt(sts.url, iam.url)).read());
  expect(refusal?.message).toBe(
    `trade this machine's credential at the security token service ${sts.url}/v1/token: HTTP 400 invalid_grant`
  );
  expect(Bun.inspect(refusal)).not.toContain(EXTERNAL_TOKEN);
  // Nothing is minted with no federated token.
  expect(iam.posted).toHaveLength(0);
});

// A 307 or 308 in front of the security token service would send the POST, the subject token in
// its body, on to another origin. Google's services answer no redirect, so the call follows none:
// the redirect is the refusal, and its target is asked nothing (the review of 81d860a8f7, P1).
test("a redirect from the security token service is refused, and its target is asked nothing", async () => {
  const elsewhere = stubService(federating);
  const moving = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () =>
      new Response(null, { status: 307, headers: { Location: `${elsewhere.url}/v1/token` } }),
  });
  stops.push(() => moving.stop(true));
  const iam = stubService(minting);
  const refusal = await refusalOf(async () =>
    (await poolAt(`http://127.0.0.1:${moving.port}`, iam.url)).read()
  );
  expect(refusal?.message).toBe(
    `trade this machine's credential at the security token service http://127.0.0.1:${moving.port}/v1/token: HTTP 307`
  );
  expect(elsewhere.posted).toHaveLength(0);
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

test("generateIdToken refusing is an error naming it, its status and its code", async () => {
  const reason = JSON.stringify({
    error: {
      code: 403,
      message: `Permission iam.serviceAccounts.getOpenIdToken denied for ${FEDERATED_TOKEN}`,
      status: "PERMISSION_DENIED",
    },
  });
  const sts = stubService(federating);
  const iam = stubService(() => ({ status: 403, body: reason }));
  const refusal = await refusalOf(async () => (await poolAt(sts.url, iam.url)).read());
  expect(refusal?.message).toBe(
    `mint this machine's identity token at ${iam.url}/v1/projects/-/serviceAccounts/${SERVICE_ACCOUNT}:generateIdToken: HTTP 403 PERMISSION_DENIED`
  );
  expect(Bun.inspect(refusal)).not.toContain(FEDERATED_TOKEN);
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

// Each service is named as every client names a URL (`urlForText`): a plain URL by itself, the
// mint as the request built on IAM Credentials' plain base (`requestForText`), and a URL that
// carries user info by its fixed phrase alone, no part of it.
test("the security token service and IAM Credentials are named only where their URL is plain", async () => {
  const password = fixtureSecret("pw");
  const withUser = (url: string) => url.replace("http://", `http://operator:${password}@`);
  const unanswering = stubService(() => ({ body: { token_type: "Bearer", expires_in: 3600 } }));
  const sts = stubService(federating);
  const iam = stubService(() => ({ status: 403, body: '{"error":{"code":403}}' }));
  const empty = stubService(() => ({ body: {} }));
  const refusals = [
    await refusalOf(async () => (await poolAt(withUser(unanswering.url), iam.url)).read()),
    await refusalOf(async () => (await poolAt(sts.url, withUser(iam.url))).read()),
    await refusalOf(async () => (await poolAt(sts.url, withUser(empty.url))).read()),
  ];
  expect(refusals.map((refusal) => refusal?.message)).toEqual([
    `the security token service securityTokenServiceUrl ${URL_NOT_PRINTED} answered no access_token`,
    `mint this machine's identity token at iamCredentialsUrl ${URL_NOT_PRINTED}: HTTP 403`,
    `generateIdToken at iamCredentialsUrl ${URL_NOT_PRINTED} answered no token`,
  ]);
  for (const refusal of refusals) expect(Bun.inspect(refusal)).not.toContain(password);
});

// A request that got no answer is named by its error's class, since the runtime's message repeats
// the host it was handed, and no error keeps another as its cause. A name that does not resolve is
// answered by a stand-in fetch that throws what Bun's own fetch throws for one (measured on Bun
// 1.4.4: a TypeError `getaddrinfo ENOTFOUND <host>`, with that code and syscall), so the row asks
// no resolver and its verdict is the naming, not how fast the machine's DNS answers. A refused
// connection is the runtime's own, on the loopback, where no name is looked up.
test("a service that does not answer is named by the class of the failure, with no cause", async () => {
  const host = `${randomBytes(9).toString("hex")}.invalid`;
  const asked: string[] = [];
  const unresolved = (async (input: string | URL | Request): Promise<Response> => {
    asked.push(String(input));
    throw Object.assign(new TypeError(`getaddrinfo ENOTFOUND ${host}`), {
      code: "ENOTFOUND",
      syscall: "getaddrinfo",
      hostname: host,
      errno: 4,
    });
  }) as typeof fetch;
  const iam = stubService(minting);
  const refusal = await refusalOf(async () =>
    (await poolAt(`http://${host}`, iam.url, unresolved)).read()
  );
  expect(asked).toEqual([`http://${host}/v1/token`]);
  expect(refusal?.message).toBe(
    `trade this machine's credential at the security token service http://${host}/v1/token: no answer: getaddrinfo ENOTFOUND`
  );
  expect(refusal?.cause).toBeUndefined();
  expect(iam.posted).toHaveLength(0);
  const sts = stubService(federating);
  const lost = await refusalOf(async () => (await poolAt(sts.url, "http://127.0.0.1:1")).read());
  expect(lost?.message).toBe(
    `mint this machine's identity token at http://127.0.0.1:1/v1/projects/-/serviceAccounts/${SERVICE_ACCOUNT}:generateIdToken: no answer: ECONNREFUSED`
  );
  expect(lost?.cause).toBeUndefined();
});

test("an external credential that cannot be read stops the pool before either service is asked", async () => {
  const sts = stubService(federating);
  const iam = stubService(minting);
  const missing = join(makeDirectory(), "absent");
  const source = poolIdentity({
    subject: projectedToken(missing, "AGENT_SECRETS_MACHINE_TOKEN_FILE"),
    audience: POOL_AUDIENCE,
    serviceAccount: SERVICE_ACCOUNT,
    tokenAudience: CLIENT_ID,
    securityTokenServiceUrl: `${sts.url}/v1/token`,
    iamCredentialsUrl: iam.url,
  });
  const refusal = await refusalOf(() => source.read());
  expect(refusal?.message).toBe(
    "read the credential the workload identity pool federates: read the projected service-account token AGENT_SECRETS_MACHINE_TOKEN_FILE names: ENOENT"
  );
  expect(refusal?.message).not.toContain(missing);
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
      clientSecret: SIGNIN_CLIENT_SECRET,
    }),
    actor: await poolAt(sts, iam),
    session: "ses_01a9",
    log: () => {},
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

// An identity source's failure is traded again on the retry loop's long schedule, as the Go
// client trades again on every failure but the broker's permanent refusals, so a caller is told it
// at its deadline, in the source's own words (`TradeRetryingError`).
test("with the security token service refusing, a person's trade fails naming it and the broker is asked nothing", async () => {
  const sts = stubService(() => ({ status: 400, body: '{"error":"invalid_grant"}' }));
  const iam = stubService(minting);
  const broker = stubBroker(() => ({ body: accepted("never", 600) }));
  stops.push(broker.stop);
  const { client } = await personOnPoolMachine(sts.url, iam.url, broker.url);
  stops.push(() => client.close());
  const refusal = await refusalOf(() => client.token(AbortSignal.timeout(1_000)));
  expect(refusal).toBeInstanceOf(TradeRetryingError);
  expect(refusal?.message).toContain(
    `the security token service ${sts.url}/v1/token: HTTP 400 invalid_grant`
  );
  expect(broker.trades).toHaveLength(0);
  expect(iam.posted).toHaveLength(0);
});

test("with generateIdToken refusing, a person's trade fails naming it and the broker is asked nothing", async () => {
  const reason = '{"error":{"code":403,"message":"Permission denied"}}';
  const sts = stubService(federating);
  const iam = stubService(() => ({ status: 403, body: reason }));
  const broker = stubBroker(() => ({ body: accepted("never", 600) }));
  stops.push(broker.stop);
  const { client } = await personOnPoolMachine(sts.url, iam.url, broker.url);
  stops.push(() => client.close());
  const refusal = await refusalOf(() => client.token(AbortSignal.timeout(1_000)));
  expect(refusal).toBeInstanceOf(TradeRetryingError);
  expect(refusal?.message).toContain(
    `mint this machine's identity token at ${iam.url}/v1/projects/-/serviceAccounts/${SERVICE_ACCOUNT}:generateIdToken: HTTP 403`
  );
  // The security token service answered and generateIdToken refused: no trade reaches the broker
  // without the machine token that vouches for the person.
  expect(sts.posted).toHaveLength(1);
  expect(broker.trades).toHaveLength(0);
});
