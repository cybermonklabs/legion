// packages/cipher-client/src/__tests__/person-identity.test.ts
//
// A person's own identity token as the subject of a trade: renewed at their issuer from the
// refresh token their sign-in keeps, at every read, and ended for good once the issuer no longer
// honours it.

import { afterEach, expect, test } from "bun:test";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { type IssuerAnswer, signedIn, stubIssuer } from "../../test/sign-ins";
import { personIdentity } from "../person-identity";
import { SIGNIN_COMMAND, type SignInFlow } from "../signins";
import { isFinalRefusal, SignInEndedError } from "../token-errors";
import { SUBJECT_TOKEN_TYPE_PERSON } from "../wire";
import { refusalOf } from "./stub";

const PERSON = "ada@example.com";
const CLIENT_ID = "example-device-client";
const CLIENT_SECRET = fixtureSecret("client");

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
    body: { access_token: fixtureSecret("issuer"), token_type: "Bearer", expires_in: 3600 },
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
  // No cause: the issuer's answer hangs off the library's error, printed with an error printed
  // whole.
  expect(refusal?.cause).toBeUndefined();
});

// An issuer, or anything at AGENT_SECRETS_SIGNIN_ISSUER, can put the refresh token and the client
// secret it was sent in its refusal's text, so the text is withheld and the status and code name
// the refusal (#4140, S1).
test("an issuer's refusal prints its status and code, never the text that echoes what it was sent", async () => {
  const record = await signedIn(PERSON);
  const issuer = await issuerFor(() => ({
    status: 401,
    body: {
      error: "invalid_client",
      error_description: `refused ${record.refresh_token} with ${CLIENT_SECRET}`,
    },
  }));
  const refusal = await refusalOf(() => personIdentity(record, flowAt(issuer.url)).read());
  expect(refusal?.message).toBe(
    `renew ${PERSON}'s sign-in at ${issuer.url}: HTTP 401 invalid_client: (its text is withheld)`
  );
  expect(Bun.inspect(refusal)).not.toContain(record.refresh_token);
  expect(Bun.inspect(refusal)).not.toContain(CLIENT_SECRET);
});

// The issuer is named as every client names a URL (`urlForText`): one that carries user info by
// its variable's fixed phrase alone, no part of it. No error keeps the library's as its cause:
// openid-client's error for an issuer that is not the one asked for holds the issuer as it was
// configured, its user info included.
test("an issuer with user info is named by its variable alone, in its text and its cause", async () => {
  const issuer = await issuerFor();
  const password = fixtureSecret("pw");
  const withUser = issuer.url.replace("http://", `http://operator:${password}@`);
  const refusal = await refusalOf(async () =>
    personIdentity(await signedIn(PERSON), flowAt(withUser)).read()
  );
  expect(refusal?.message).toStartWith(
    `read the sign-in issuer AGENT_SECRETS_SIGNIN_ISSUER ${URL_NOT_PRINTED}: `
  );
  expect(refusal?.cause).toBeUndefined();
  expect(Bun.inspect(refusal)).not.toContain(password);
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

// Only the issuer's 400 or 401 invalid_grant (RFC 6749 section 5.2) ends a sign-in, the rule the
// Go client's DeviceFlow.Refresh and Dispatch's own refresh read by: a 429 or a 5xx is the issuer
// busy or down, and a 403 no refusal of the grant, whatever the body names, and reading one as the
// end would drop a live token and stop the client over an outage.
test.each([
  [400, true],
  [401, true],
  [403, false],
  [429, false],
  [500, false],
  [503, false],
])("an issuer's %i naming invalid_grant ends the sign-in: %p", async (status, ended) => {
  const issuer = await issuerFor(() => ({ status, body: { error: "invalid_grant" } }));
  const refusal = await refusalOf(async () =>
    personIdentity(await signedIn(PERSON), flowAt(issuer.url)).read()
  );
  expect(refusal instanceof SignInEndedError).toBe(ended);
  expect(isFinalRefusal(refusal)).toBe(ended);
});

test("a discovery that failed is not kept: the next read asks the issuer again", async () => {
  const issuer = await issuerFor();
  let calls = 0;
  // As Bun's fetch fails: a code beside a message, which can repeat the host it was handed.
  const flaky = (async (input: string | URL | Request, init?: RequestInit) => {
    calls += 1;
    if (calls === 1)
      throw Object.assign(new TypeError("connect to canary-host.invalid refused"), {
        code: "ECONNREFUSED",
      });
    return await fetch(input, init);
  }) as typeof fetch;
  const source = personIdentity(await signedIn(PERSON), flowAt(issuer.url), { fetch: flaky });
  const refusal = await refusalOf(() => source.read());
  expect(refusal?.message).toStartWith(`read the sign-in issuer ${issuer.url}: `);
  // The reason the request got no answer, which the library hangs off its own error, by its class.
  expect(refusal?.message).toContain("ECONNREFUSED");
  expect(refusal?.message).not.toContain("canary-host");
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
