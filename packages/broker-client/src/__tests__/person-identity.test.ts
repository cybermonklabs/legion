// packages/broker-client/src/__tests__/person-identity.test.ts
//
// A person's own identity token as the subject of a trade: renewed at their issuer from the
// refresh token their sign-in keeps, at every read, and ended for good once the issuer no longer
// honours it.

import { afterEach, expect, test } from "bun:test";
import { type IssuerAnswer, signedIn, stubIssuer } from "../../test/sign-ins";
import { personIdentity } from "../person-identity";
import { SIGNIN_COMMAND, type SignInFlow } from "../signins";
import { isFinalRefusal, SignInEndedError } from "../token-client";
import { SUBJECT_TOKEN_TYPE_PERSON } from "../wire";
import { refusalOf } from "./stub";

const PERSON = "ada@example.com";
const CLIENT_ID = "example-device-client";
const CLIENT_SECRET = "example-device-client-not-a-secret";

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A stub issuer for {@link PERSON}'s sign-in with {@link CLIENT_ID}, stopped after the test. */
async function issuerFor(answer?: (count: number) => IssuerAnswer | undefined) {
  const issuer = await stubIssuer({ clientId: CLIENT_ID, person: PERSON, answer });
  stops.push(issuer.stop);
  return issuer;
}

/** The sign-in client the deployment configures, at `issuer`. */
function flowAt(issuer: string): SignInFlow {
  return { issuer, clientId: CLIENT_ID, clientSecret: CLIENT_SECRET };
}

test("each read refreshes once, with the client in the form, and answers the person's ID token", async () => {
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: CLIENT_ID });
  const source = personIdentity(record, flowAt(issuer.url));
  const first = await source.read();
  const second = await source.read();
  expect(first).toEqual({
    token: issuer.idTokens[0] as string,
    tokenType: SUBJECT_TOKEN_TYPE_PERSON,
  });
  expect(second).toEqual({
    token: issuer.idTokens[1] as string,
    tokenType: SUBJECT_TOKEN_TYPE_PERSON,
  });
  // The discovery document once per source, then one refresh per read.
  expect(issuer.requests.map((request) => request.path)).toEqual([
    "/.well-known/openid-configuration",
    "/token",
    "/token",
  ]);
  for (const refresh of issuer.requests.slice(1)) {
    expect([...refresh.form.keys()].sort()).toEqual([
      "client_id",
      "client_secret",
      "grant_type",
      "refresh_token",
    ]);
    expect(refresh.form.get("grant_type")).toBe("refresh_token");
    expect(refresh.form.get("refresh_token")).toBe(record.refresh_token);
    expect(refresh.form.get("client_id")).toBe(CLIENT_ID);
    expect(refresh.form.get("client_secret")).toBe(CLIENT_SECRET);
    expect(refresh.authorization).toBeNull();
  }
  expect(source.describe()).toBe("ada@example.com's sign-in on this machine");
});

test("an answer with no ID token is an error", async () => {
  const issuer = await issuerFor(() => ({
    body: { access_token: "issuer-access-not-a-secret", token_type: "Bearer", expires_in: 3600 },
  }));
  const source = personIdentity(await signedIn(PERSON), flowAt(issuer.url));
  const refusal = await refusalOf(() => source.read());
  expect(refusal?.message).toBe(
    `${issuer.url} answered no id_token: the sign-in asked for the "openid" scope and must be answered one`
  );
  expect(isFinalRefusal(refusal)).toBe(false);
});

test("a refresh token the issuer no longer honours ends the sign-in, naming agent-secrets login", async () => {
  const issuer = await issuerFor(() => ({
    status: 400,
    body: { error: "invalid_grant", error_description: "Token has been expired or revoked." },
  }));
  const record = await signedIn(PERSON);
  const refusal = await refusalOf(() => personIdentity(record, flowAt(issuer.url)).read());
  expect(refusal).toBeInstanceOf(SignInEndedError);
  expect(refusal?.message).toBe(
    `${issuer.url} no longer honours ada@example.com's sign-in on this machine; run: ${SIGNIN_COMMAND}`
  );
  expect(isFinalRefusal(refusal)).toBe(true);
  expect(refusal?.message).not.toContain(record.refresh_token);
  expect(refusal?.message).not.toContain(CLIENT_SECRET);
});

test("an issuer that is down is an ordinary error, and the next read asks again", async () => {
  const issuer = await issuerFor((count) =>
    count === 1 ? { status: 503, body: "Service Unavailable" } : undefined
  );
  const record = await signedIn(PERSON);
  const source = personIdentity(record, flowAt(issuer.url));
  const refusal = await refusalOf(() => source.read());
  expect(refusal).not.toBeInstanceOf(SignInEndedError);
  expect(isFinalRefusal(refusal)).toBe(false);
  expect(refusal?.message).toStartWith(
    `renew ada@example.com's sign-in at ${issuer.url}: HTTP 503`
  );
  expect(refusal?.message).not.toContain(record.refresh_token);
  expect(refusal?.message).not.toContain(CLIENT_SECRET);
  expect((await source.read()).tokenType).toBe(SUBJECT_TOKEN_TYPE_PERSON);
});

test("a discovery that failed is not kept: the next read asks the issuer again", async () => {
  const issuer = await issuerFor();
  let calls = 0;
  const flaky = (async (input: string | URL | Request, init?: RequestInit) => {
    calls += 1;
    if (calls === 1) throw new Error("connection refused");
    return await fetch(input, init);
  }) as typeof fetch;
  const source = personIdentity(await signedIn(PERSON), flowAt(issuer.url), { fetch: flaky });
  const refusal = await refusalOf(() => source.read());
  expect(refusal?.message).toStartWith(`read the sign-in issuer ${issuer.url}: `);
  // The reason the request got no answer, which the library hangs off its own error.
  expect(refusal?.message).toContain("connection refused");
  expect((await source.read()).tokenType).toBe(SUBJECT_TOKEN_TYPE_PERSON);
  expect(issuer.requests.map((request) => request.path)).toEqual([
    "/.well-known/openid-configuration",
    "/token",
  ]);
});

test("a record made with another sign-in client is refused before any request", async () => {
  const issuer = await issuerFor();
  const record = await signedIn(PERSON, { client_id: "another-device-client" });
  expect(() => personIdentity(record, flowAt(issuer.url))).toThrow(
    `ada@example.com signed in with another client than this machine now has (another-device-client), so their sign-in cannot be renewed; run: ${SIGNIN_COMMAND}`
  );
  expect(issuer.requests).toHaveLength(0);
  // A record that names no client is renewed with the one this machine has, as Go's is.
  const unnamed = personIdentity(await signedIn(PERSON, { client_id: "" }), flowAt(issuer.url));
  expect((await unnamed.read()).tokenType).toBe(SUBJECT_TOKEN_TYPE_PERSON);
});
