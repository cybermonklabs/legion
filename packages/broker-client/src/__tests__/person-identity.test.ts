// packages/broker-client/src/__tests__/person-identity.test.ts
//
// A person's own identity token as the subject of a trade: renewed at their issuer from the
// refresh token their sign-in keeps, at every read, and ended for good once the issuer no longer
// honours it.

import { afterEach, expect, test } from "bun:test";
import { inspect } from "node:util";
import { URL_NOT_PRINTED } from "@legion/contracts/log-text";
import { unshownSettingUrl } from "@legion/contracts/setting-url";
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
// its variable's fixed phrase alone, no part of it. The read refuses such an issuer before its
// discovery, since an issuer's identifier holds no user info, and no error keeps a cause:
// openid-client's error for an issuer that is not the one asked for holds the issuer as it was
// configured, its user info included.
test("an issuer with user info is named by its variable alone, in its text and its cause", async () => {
  const issuer = await issuerFor();
  const password = fixtureSecret("pw");
  const withUser = issuer.url.replace("http://", `http://operator:${password}@`);
  const refusal = await refusalOf(async () =>
    personIdentity(await signedIn(PERSON), flowAt(withUser)).read()
  );
  expect(refusal?.message).toBe(
    `AGENT_SECRETS_SIGNIN_ISSUER ${URL_NOT_PRINTED} must carry no user and no password: an issuer's identifier holds neither, so its discovery cannot pass with them`
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

// AGENT_SECRETS_SIGNIN_ISSUER may carry a user and a password, which an issuer's identifier never
// holds, so its discovery cannot pass with one: the read refuses such an issuer before anything is
// asked, naming it by its variable's phrase alone (`urlForText`). An issuer that is no http or
// https URL with a host is refused naming its length and the cause the one function found. No
// member of the error, nor of a cause under it, holds the password. The password is made for this
// run.
test("an issuer with a user part is refused by its variable's phrase, and one that is no http or https URL with a host by its length and cause", async () => {
  const password = crypto.randomUUID();
  const issuer = await issuerFor();
  const withUser = issuer.url.replace("http://", `http://made-up-user:${password}@`);
  let calls = 0;
  const refused = (async () => {
    calls += 1;
    throw new Error("connection refused");
  }) as unknown as typeof fetch;
  const record = await signedIn(PERSON);
  const said: { readonly message: string; readonly holds: boolean }[] = [];
  const unreadable = [
    `made-up-user:${password}@issuer.example`,
    `http://made-up-user:${password}@[bad`,
  ];
  for (const named of [withUser, ...unreadable]) {
    const error = await refusalOf(() =>
      personIdentity(record, flowAt(named), { fetch: refused }).read()
    );
    said.push({
      message: error instanceof Error ? error.message : String(error),
      holds: inspect(error, { depth: Number.POSITIVE_INFINITY, showHidden: true }).includes(
        password
      ),
    });
  }
  expect(said).toEqual([
    {
      message: `AGENT_SECRETS_SIGNIN_ISSUER ${URL_NOT_PRINTED} must carry no user and no password: an issuer's identifier holds neither, so its discovery cannot pass with them`,
      holds: false,
    },
    ...unreadable.map((value) => ({
      message: `AGENT_SECRETS_SIGNIN_ISSUER must name an http or https URL: ${unshownSettingUrl(value)}`,
      holds: false,
    })),
  ]);
  // None of the three was asked anything.
  expect(calls).toBe(0);
});

// With a real fetch against a stub issuer, an issuer with a user part would reach discovery, and
// the library's error for the issuer its metadata names keeps the expected issuer, password
// included, deep in its cause chain. The read refuses it before discovery: the issuer is asked
// nothing, and no member of the error, nor of any cause under it, nor `inspect` of the whole
// error, holds the password. The password is made for this run.
test("an issuer with a user part asks the real issuer nothing and holds its password nowhere in the error", async () => {
  const password = crypto.randomUUID();
  const issuer = await issuerFor();
  const withUser = issuer.url.replace("http://", `http://made-up-user:${password}@`);
  const record = await signedIn(PERSON);
  const error = await refusalOf(() => personIdentity(record, flowAt(withUser)).read());
  const holding: string[] = [];
  let depth = 0;
  for (let at: unknown = error; at instanceof Error; at = at.cause, depth += 1) {
    for (const key of Object.getOwnPropertyNames(at)) {
      if (inspect(Reflect.get(at, key), { depth: Number.POSITIVE_INFINITY }).includes(password)) {
        holding.push(`${"cause.".repeat(depth)}${key}`);
      }
    }
  }
  expect({
    holding,
    inspected: inspect(error, { depth: Number.POSITIVE_INFINITY, showHidden: true }).includes(
      password
    ),
    asked: issuer.requests.map((request) => request.path),
  }).toEqual({ holding: [], inspected: false, asked: [] });
});
