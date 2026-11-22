// packages/envoy-client/src/__tests__/broker-credential-signins.test.ts
//
// The credential where a person signed in on the machine: that it trades that person under the
// right session name, apart from the session the machine trades its own identity under, that a
// client it failed to make is not kept, that it remembers a sign-in the broker or the issuer
// refused for good, and that it never trades the machine in the person's place, unless the
// process turned the person trade off. With nobody signed in, both callers trade the machine.

import { expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { rm } from "node:fs/promises";
import * as path from "node:path";
import {
  PERSON_TRADE_VAR,
  type SignInRecord,
  signInSessionName,
} from "@legion/cipher-client/signins";
import {
  type IssuerAnswer,
  signedIn,
  signInPath,
  stubIssuer,
  thumbprintOf,
  writeSignIn,
} from "@legion/cipher-client/test/sign-ins";
import {
  SUBJECT_TOKEN_TYPE_KUBERNETES,
  SUBJECT_TOKEN_TYPE_PERSON,
} from "@legion/cipher-client/wire";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { forgetBrokerClients } from "../broker-credential";
import { type DispatchEnvironment, dispatchSettings } from "../dispatch-config";
import { DispatchClient, DispatchWriteUnsettledError } from "../dispatch-http";
import { envoySettings } from "../envoy-settings";
import { brokerStandIns } from "./broker-stand-ins";

const { noFile, podEnvironment, projectedToken, servers, stubBroker, stubDispatch, tempDir } =
  brokerStandIns("broker-credential-signins");

const PERSON = "ada@example.com";
const CLIENT_ID = "example-device-client";
/** The name this machine trades its own identity under, as a daemon's environment sets it. */
const MACHINE_SESSION = "legion-demo-controller";
/** What the broker answers a trade proved with the box key of a sign-in its person revoked. */
const REVOKED = `${PERSON} revoked this machine's sign-in, so the key this proof was signed with trades nothing. Sign in again with \`agent-secrets login\`.`;

/** One trade the stub broker was asked, the thumbprint of the key its proof was signed with, and
 *  the access token it answered, none where it refused the trade. */
interface ProvedTrade {
  readonly form: URLSearchParams;
  readonly thumbprint: string;
  readonly token: string | undefined;
}

/**
 * A broker that mints a random token good for ten minutes for every trade, kept with the trade,
 * except a trade proved with a key whose thumbprint `revoked` holds, which it refuses as it
 * refuses a sign-in its person revoked, and one proved with a key `away` holds, which it answers
 * 503, as a broker that is down for that one.
 */
function signInBroker(): {
  url: string;
  trades: ProvedTrade[];
  revoked: Set<string>;
  away: Set<string>;
} {
  const trades: ProvedTrade[] = [];
  const revoked = new Set<string>();
  const away = new Set<string>();
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const header = (request.headers.get("DPoP") ?? "").split(".")[0] ?? "";
      const { jwk } = JSON.parse(Buffer.from(header, "base64url").toString("utf8"));
      const thumbprint = await thumbprintOf(jwk);
      if (away.has(thumbprint)) {
        trades.push({
          form: new URLSearchParams(await request.text()),
          thumbprint,
          token: undefined,
        });
        return new Response("the broker is away", { status: 503 });
      }
      const token = revoked.has(thumbprint) ? undefined : fixtureSecret("broker");
      trades.push({ form: new URLSearchParams(await request.text()), thumbprint, token });
      if (token === undefined) {
        return Response.json(
          { error: "invalid_grant", error_description: REVOKED },
          { status: 400 }
        );
      }
      return Response.json({
        access_token: token,
        issued_token_type: "urn:ietf:params:oauth:token-type:access_token",
        token_type: "DPoP",
        expires_in: 600,
      });
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}`, trades, revoked, away };
}

/** The issuer {@link PERSON}'s sign-in is renewed at, stopped after the test; `answer` replaces
 *  a refresh's answer where it gives one. */
async function issuerFor(answer?: (count: number) => IssuerAnswer | undefined) {
  const issuer = await stubIssuer({
    clientId: CLIENT_ID,
    person: PERSON,
    ...(answer === undefined ? {} : { answer }),
  });
  servers.push(issuer);
  return issuer;
}

/**
 * The environment of a pod whose sign-in directory (`signins`) holds `records`, with the sign-in
 * client at `issuerUrl` configured and `AGENT_SECRETS_SESSION` naming the machine's own session,
 * as a daemon's environment sets it. The pod's projected token is the actor: which kind of
 * machine a person signs in on changes nothing this file reads.
 */
async function signedInPod(
  brokerUrl: string,
  dispatchUrl: string,
  issuerUrl: string,
  records: readonly SignInRecord[]
): Promise<{ env: DispatchEnvironment; signins: string }> {
  const signins = path.join(tempDir(), "signins");
  for (const record of records) await writeSignIn(signins, record);
  const secret = path.join(tempDir(), "client-secret");
  // Random, as a real client secret is: one spelled from words would be scrubbed out of the lines
  // that hold those words (`@legion/contracts/held-secrets`).
  writeFileSync(secret, `secret-${crypto.randomUUID()}\n`, { mode: 0o600 });
  return {
    env: {
      ...podEnvironment(brokerUrl, dispatchUrl),
      AGENT_SECRETS_SESSION: MACHINE_SESSION,
      AGENT_SECRETS_SIGNIN_DIR: signins,
      AGENT_SECRETS_SIGNIN_ISSUER: issuerUrl,
      AGENT_SECRETS_SIGNIN_CLIENT_ID: CLIENT_ID,
      AGENT_SECRETS_SIGNIN_CLIENT_SECRET_FILE: secret,
    },
    signins,
  };
}

/** A Dispatch client of `env` for a caller acting as `session`, or as none. */
function dispatchOf(env: DispatchEnvironment, session?: string): DispatchClient {
  const resolution = dispatchSettings(env, noFile(env), session === undefined ? {} : { session });
  if (!resolution.enabled) throw new Error(`Dispatch is off: ${resolution.error}`);
  return new DispatchClient(resolution.url, resolution.credential);
}

/** The words `call` failed with; a call that succeeds fails the test. */
async function refusalOf(call: Promise<unknown>): Promise<string> {
  return await call.then(
    () => {
      throw new Error("expected the call to be refused");
    },
    (error: unknown) => (error instanceof Error ? error.message : String(error))
  );
}

test("a person's trade asks under the caller's own session, else the sign-in's name, never AGENT_SECRETS_SESSION", async () => {
  // The machine trades under AGENT_SECRETS_SESSION, and the broker holds a name for one identity,
  // so the person asking under it would be refused.
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);

  await dispatchOf(env).listAllIssues({});
  await dispatchOf(env, "ses_01a9").listAllIssues({});

  expect(
    broker.trades.map((trade) => [
      trade.form.get("subject_token_type"),
      trade.form.get("session"),
      trade.thumbprint,
    ])
  ).toEqual([
    [SUBJECT_TOKEN_TYPE_PERSON, signInSessionName(record), record.thumbprint],
    [SUBJECT_TOKEN_TYPE_PERSON, "ses_01a9", record.thumbprint],
  ]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[0]?.token}`,
    `DPoP ${broker.trades[1]?.token}`,
  ]);
});

test("a caller acting as no session and one acting as AGENT_SECRETS_SESSION's session trade apart, whichever calls first", async () => {
  // With a sign-in, a caller acting as no session trades under the sign-in's own name and one
  // acting as the session AGENT_SECRETS_SESSION names trades under that session. One token for
  // both would name one of them wrongly, whichever called first, and the listener refuses every
  // write of a session whose token names another (`sessionNotBound`).
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);
  const asNone = () => dispatchOf(env).listAllIssues({});
  const asMachineSession = () => dispatchOf(env, MACHINE_SESSION).listAllIssues({});

  await asNone();
  await asMachineSession();
  await asNone();
  await asMachineSession();
  // A process that has traded nothing yet, whose first caller is the other one.
  forgetBrokerClients();
  await asMachineSession();
  await asNone();
  await asMachineSession();
  await asNone();

  expect(broker.trades.map((trade) => trade.form.get("session"))).toEqual([
    signInSessionName(record),
    MACHINE_SESSION,
    MACHINE_SESSION,
    signInSessionName(record),
  ]);
  // Every call presents the token its own caller's trade was answered.
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[0]?.token}`,
    `DPoP ${broker.trades[1]?.token}`,
    `DPoP ${broker.trades[0]?.token}`,
    `DPoP ${broker.trades[1]?.token}`,
    `DPoP ${broker.trades[2]?.token}`,
    `DPoP ${broker.trades[3]?.token}`,
    `DPoP ${broker.trades[2]?.token}`,
    `DPoP ${broker.trades[3]?.token}`,
  ]);
});

test("with nobody signed in, a caller acting as no session and one acting as AGENT_SECRETS_SESSION's session both trade the machine under it", async () => {
  const broker = stubBroker(600);
  const dispatch = stubDispatch();
  const env = {
    ...podEnvironment(broker.url, dispatch.url),
    AGENT_SECRETS_SESSION: MACHINE_SESSION,
  };

  await dispatchOf(env).listAllIssues({});
  await dispatchOf(env, MACHINE_SESSION).listAllIssues({});
  forgetBrokerClients();
  await dispatchOf(env, MACHINE_SESSION).listAllIssues({});
  await dispatchOf(env).listAllIssues({});

  // Every trade is the machine's identity under the session AGENT_SECRETS_SESSION names, and every
  // call presents a token the broker answered.
  expect(broker.trades.length).toBeGreaterThan(0);
  for (const trade of broker.trades) {
    expect([trade.form.get("subject_token"), trade.form.get("session")]).toEqual([
      projectedToken,
      MACHINE_SESSION,
    ]);
  }
  expect(dispatch.asked).toHaveLength(4);
  const answered = broker.trades.map((trade) => `DPoP ${trade.token}`);
  for (const one of dispatch.asked) {
    expect(answered).toContain(one.authorization);
  }
});

test("a client the factory failed to make is not kept: a store the person corrected is read with no restart", async () => {
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const ada = await signedIn(PERSON, { client_id: CLIENT_ID });
  const grace = await signedIn("grace@example.com", { client_id: CLIENT_ID });
  const { env, signins } = await signedInPod(broker.url, dispatch.url, issuer.url, [ada, grace]);
  const client = dispatchOf(env, "ses_01a9");

  expect(await refusalOf(client.listAllIssues({}))).toBe(
    "2 people are signed in on this machine (ada@example.com, grace@example.com), so this session must say which one it acts for: set AGENT_SECRETS_PERSON to one of them"
  );
  expect(broker.trades).toHaveLength(0);

  await rm(signInPath(signins, grace.person));
  await client.listAllIssues({});
  expect(broker.trades.map((trade) => trade.thumbprint)).toEqual([ada.thumbprint]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[0]?.token}`,
  ]);
});

test("a sign-in the broker refused for good fails every later call in its words, asking nothing and never trading the machine", async () => {
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);
  broker.revoked.add(record.thumbprint);

  const refused = await refusalOf(dispatchOf(env, "ses_01a9").listAllIssues({}));
  expect(refused).toBe(
    `the secrets broker refused ${PERSON}'s sign-in on this machine: HTTP 400 invalid_grant: ${REVOKED}`
  );
  const asked = issuer.requests.length;

  // The same session's later calls, Dispatch's and the listener's alike, are refused in the same
  // words with no refresh at the issuer, no trade at the broker and no request to Dispatch.
  expect(await refusalOf(dispatchOf(env, "ses_01a9").listAllIssues({}))).toBe(refused);
  const listener = envoySettings(env, noFile(env), { nats: false, session: "ses_01a9" });
  if (!listener.enabled) throw new Error("Envoy is off");
  expect(
    await refusalOf(
      Promise.resolve(listener.credential?.headers("POST", `${dispatch.url}/v1/roles/set`))
    )
  ).toBe(refused);
  expect(issuer.requests).toHaveLength(asked);
  expect(dispatch.asked).toHaveLength(0);
  // The one trade made is the person's: the machine is never traded in the person's place.
  expect(broker.trades.map((trade) => trade.form.get("subject_token_type"))).toEqual([
    SUBJECT_TOKEN_TYPE_PERSON,
  ]);
});

test("after a refusal, a new sign-in with a new box key gets a client of its own, which trades with that key", async () => {
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env, signins } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);
  broker.revoked.add(record.thumbprint);
  const client = dispatchOf(env, "ses_01a9");
  expect(await refusalOf(client.listAllIssues({}))).toContain("agent-secrets login");

  // `agent-secrets login` again: a new box key, written over the person's record.
  const renewed = await signedIn(PERSON, { client_id: CLIENT_ID });
  await writeSignIn(signins, renewed);
  await client.listAllIssues({});

  expect(broker.trades.map((trade) => trade.thumbprint)).toEqual([
    record.thumbprint,
    renewed.thumbprint,
  ]);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[1]?.token}`,
  ]);
});

// T1 (the review of 4dad905e68): a write that may have landed presents as no other login than its
// attempts went out as, the sign-in half included. Once a sign-in was refused for good, every call
// asks the store again (`clientFor`), so a sign-in signed out between two attempts of one write
// sends the next presentation as the machine: another login to Dispatch (`requestKeyLogin`: the
// service, not the person), whose key would run the write a second time. The write ends unsettled
// with its key, and Dispatch is asked once.
test("a write whose person signs out between two attempts ends unsettled, presenting nothing as the machine", async () => {
  const broker = signInBroker();
  const issuer = await issuerFor();
  const revoked = await signedIn(PERSON, { client_id: CLIENT_ID });
  const asked: { readonly authorization: string; readonly key: string }[] = [];
  let signins = "";
  let renewed: SignInRecord | undefined;
  const dispatch = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const key = request.headers.get("Idempotency-Key") ?? "";
      asked.push({ authorization: request.headers.get("Authorization") ?? "", key });
      if (request.method === "POST" && asked.length === 1) {
        // The person signs out while Dispatch answers the write's first attempt.
        await rm(signInPath(signins, (renewed as SignInRecord).person));
        return Response.json(
          { code: "UNAVAILABLE", error: "the database is restarting" },
          { status: 503, headers: { "Idempotency-Key": key } }
        );
      }
      return Response.json(
        { comment: { id: "c-1" } },
        { status: 201, headers: { "Idempotency-Key": key } }
      );
    },
  });
  servers.push(dispatch);
  const pod = await signedInPod(broker.url, `http://127.0.0.1:${dispatch.port}`, issuer.url, [
    revoked,
  ]);
  signins = pod.signins;
  // A sign-in refused for good, so every later call reads the store again.
  broker.revoked.add(revoked.thumbprint);
  const client = dispatchOf(pod.env, "ses_01a9");
  expect(await refusalOf(client.listAllIssues({}))).toContain("agent-secrets login");
  // Signed in again: the write's first attempt goes as the person.
  renewed = await signedIn(PERSON, { client_id: CLIENT_ID });
  await writeSignIn(signins, renewed);

  const error = await client
    .comment("DSP-1", { body: "Shipped.", actor: { kind: "session", id: "ses_01a9" } })
    .then(
      () => undefined,
      (caught: unknown) => caught
    );

  expect(error).toBeInstanceOf(DispatchWriteUnsettledError);
  expect((error as DispatchWriteUnsettledError).key).toBe(asked[0]?.key as string);
  expect((error as DispatchWriteUnsettledError).last.message).toContain(
    "would present as another identity"
  );
  expect(asked).toHaveLength(1);
  // The revoked sign-in's trade, the renewed one's, which the attempt went out as, and then the
  // machine's, whose token never reached Dispatch.
  expect(
    broker.trades.map((trade) => [trade.form.get("subject_token_type"), trade.thumbprint])
  ).toEqual([
    [SUBJECT_TOKEN_TYPE_PERSON, revoked.thumbprint],
    [SUBJECT_TOKEN_TYPE_PERSON, renewed.thumbprint],
    [SUBJECT_TOKEN_TYPE_KUBERNETES, broker.trades[2]?.thumbprint],
  ]);
  expect(asked[0]?.authorization).toBe(`DPoP ${broker.trades[1]?.token}`);
}, 30_000);

test("a new sign-in closes the client it replaces, so a client whose trades were failing trades no more", async () => {
  // A client's retry loop trades again for as long as the process runs. The credential replaces
  // a client when the sign-in it trades is no longer the one the machine chooses, after a sign-in
  // refused for good; the client it replaced would go on trading its sign-in, at 15 s doubling to
  // 5 minutes, for nothing, so the credential closes it.
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const revoked = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env, signins } = await signedInPod(broker.url, dispatch.url, issuer.url, [revoked]);
  broker.revoked.add(revoked.thumbprint);
  const client = dispatchOf(env, "ses_01a9");
  expect(await refusalOf(client.listAllIssues({}))).toContain("agent-secrets login");
  // Signed in again, with a key the broker is down for.
  const failing = await signedIn(PERSON, { client_id: CLIENT_ID });
  broker.away.add(failing.thumbprint);
  await writeSignIn(signins, failing);
  expect(await refusalOf(client.listAllIssues({}))).toContain("HTTP 503");
  // Signed in once more: that sign-in's client replaces the failing one's.
  const working = await signedIn(PERSON, { client_id: CLIENT_ID });
  await writeSignIn(signins, working);
  await client.listAllIssues({});
  const tradesOf = (thumbprint: string) =>
    broker.trades.filter((trade) => trade.thumbprint === thumbprint);
  expect(tradesOf(working.thumbprint)).toHaveLength(1);
  expect(tradesOf(failing.thumbprint)).toHaveLength(1);
  // Past the failing client's next slot, 15 s at its longest: it trades no more.
  await Bun.sleep(16_000);
  expect(tradesOf(failing.thumbprint)).toHaveLength(1);
}, 40_000);

test("a sign-in the issuer has ended is remembered too: no trade at the broker, and no second refresh", async () => {
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor(() => ({
    status: 400,
    body: { error: "invalid_grant", error_description: "Token has been expired or revoked." },
  }));
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);

  const ended = await refusalOf(dispatchOf(env, "ses_01a9").listAllIssues({}));
  expect(ended).toBe(
    `${issuer.url} no longer honours ${PERSON}'s sign-in on this machine; run: agent-secrets login`
  );
  const refreshes = issuer.requests.filter((one) => one.path === "/token").length;
  expect(refreshes).toBe(1);

  expect(await refusalOf(dispatchOf(env, "ses_01a9").listAllIssues({}))).toBe(ended);
  expect(issuer.requests.filter((one) => one.path === "/token")).toHaveLength(refreshes);
  expect(broker.trades).toHaveLength(0);
  expect(dispatch.asked).toHaveLength(0);
});

test("a process that turns the person trade off writes Dispatch as the machine where a person signed in, and the same process without it as the person", async () => {
  // `legion controller start` sets PERSON_SIGNIN_TRADE=off, so the controller, which keeps its
  // operator's HOME and so finds the operator's sign-in, writes Dispatch as the service.
  const broker = signInBroker();
  const dispatch = stubDispatch();
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const { env } = await signedInPod(broker.url, dispatch.url, issuer.url, [record]);

  await dispatchOf({ ...env, [PERSON_TRADE_VAR]: "off" }, "ses_01a9").listAllIssues({});
  // Nobody's sign-in was renewed for it.
  expect(issuer.requests).toHaveLength(0);
  // The setting is in the client's key, so the process without it is answered a client of its
  // own, which trades the person.
  await dispatchOf(env, "ses_01a9").listAllIssues({});

  expect(
    broker.trades.map((trade) => [
      trade.form.get("subject_token_type"),
      trade.form.get("subject_token"),
      trade.form.has("actor_token"),
      trade.form.get("session"),
    ])
  ).toEqual([
    [SUBJECT_TOKEN_TYPE_KUBERNETES, projectedToken, false, "ses_01a9"],
    [SUBJECT_TOKEN_TYPE_PERSON, issuer.idTokens[0], true, "ses_01a9"],
  ]);
  // The machine's proof is signed with a key of this process's, the person's with the box key.
  expect(broker.trades[0]?.thumbprint).not.toBe(record.thumbprint);
  expect(broker.trades[1]?.thumbprint).toBe(record.thumbprint);
  expect(dispatch.asked.map((one) => one.authorization)).toEqual([
    `DPoP ${broker.trades[0]?.token}`,
    `DPoP ${broker.trades[1]?.token}`,
  ]);
});
