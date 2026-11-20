import { expect, test } from "bun:test";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { LibraryError, refusalFetch, requestCredentials } from "./remote-text";

// requestCredentials is every value a request carries under a name no contract declares public;
// scope's value is public. Go's TestRequestTokensReadsEveryCredentialARequestCarries.
test("requestCredentials reads every value a request carries", () => {
  const basic = btoa(`client:${encodeURIComponent("basic secret/RDX")}`);
  // The URL's user information, assembled so no line of this file reads as a credential.
  const userinfo = ["user", "url-secret-RDX-7a8b"].join(":");
  const subject = fixtureSecret("subject");
  const got = requestCredentials(`https://${userinfo}@idp.example/token?code=query-code-RDX-5e6f`, {
    headers: {
      authorization: `Basic ${basic}`,
      dpop: "proof-RDX-9c0d",
      cookie: "session=cookie-RDX-4f5a",
      "content-type": "application/x-www-form-urlencoded",
    },
    body: new URLSearchParams({ subject_token: subject, scope: "openid" }),
  });
  for (const want of [
    "basic secret/RDX",
    "client",
    "proof-RDX-9c0d",
    "session=cookie-RDX-4f5a",
    "user",
    "url-secret-RDX-7a8b",
    "query-code-RDX-5e6f",
    subject,
  ]) {
    expect(got).toContain(want);
  }
  expect(got).not.toContain("openid");
  const json = requestCredentials("https://idp.example/token", {
    headers: { "content-type": "application/json" },
    body: JSON.stringify({ request: { proof: "json-proof-RDX-3c4d" } }),
  });
  expect(json).toContain("json-proof-RDX-3c4d");
});

// refusalFetch hands every answer on as it came, and records the refused request's credentials;
// a LibraryError over the library's error of that answer prints none of them, while its code
// compares as the answer gave it.
test("a refusal echoing the request prints none of its credentials, and keeps its code", async () => {
  const secret = fixtureSecret("refresh");
  const answer = JSON.stringify({
    error: "invalid_grant",
    error_description: `refresh_token ${secret} is revoked`,
  });
  const fetch = refusalFetch(async () => new Response(answer, { status: 400 }));
  const response = await fetch("https://idp.example/token", {
    headers: { "content-type": "application/x-www-form-urlencoded" },
    body: new URLSearchParams({ refresh_token: secret }),
  });
  const body = await response.text();
  expect(body).toBe(answer);
  const thrown = Object.assign(
    new Error(`server responded with an error: ${body}`),
    JSON.parse(body),
    { response }
  );
  const error = new LibraryError(thrown);
  for (const text of [error.message, String(error.code), String(error.description)]) {
    expect(text.includes(secret), `REDACT-ASSERT ts.refusal-fetch: ${text}`).toBe(false);
  }
  expect(error.code.equals("invalid_grant")).toBe(true);
});

// A refusalFetch above one that a proof is set beneath, as the broker token client's are: the
// answer is one refusal, whose record holds the proof the inner request carried and the form the
// outer one did, so a description that echoes the proof prints none of it, though the outer
// refusalFetch recorded last. Go's TestAnImpersonationRefusalPrintsNoCredentialItEchoes.
test("a refusal through two refusalFetches is one record holding both requests' credentials", async () => {
  const proof = "proof-set-between-RDX-5f6a";
  const echo = async (_url: string, options: { headers?: Record<string, string> }) =>
    new Response(
      JSON.stringify({
        error: "invalid_dpop_proof",
        error_description: `the proof ${options.headers?.dpop} is not accepted`,
      }),
      { status: 400 }
    );
  const inner = refusalFetch(echo);
  const outer = refusalFetch((url: string, options: { headers?: Record<string, string> }) =>
    inner(url, { headers: { ...options.headers, dpop: proof } })
  );
  const response = await outer("https://broker.example/token", {
    headers: { "content-type": "application/x-www-form-urlencoded" },
  });
  const body = JSON.parse(await response.text());
  const error = new LibraryError(
    Object.assign(new Error("server responded with an error"), body, { response })
  );
  for (const text of [error.message, String(error.description)]) {
    expect(text.includes(proof), `REDACT-ASSERT ts.refusal-fetch-nested: ${text}`).toBe(false);
  }
  expect(error.code.equals("invalid_dpop_proof")).toBe(true);
});

// The record is the Response the error came from, never the newest answer whose words happen to
// occur in a message. Two gateways can share a preamble and a later short "Forbidden" answer can
// occur before the first error reaches the boundary; only the first request's credential is
// redacted from that error.
test("interleaved refusals keep each error's own request record", async () => {
  const firstSecret = fixtureSecret("firstrec");
  const secondSecret = fixtureSecret("secondre");
  const first = refusalFetch(
    async () => new Response("gateway preamble shared by every refusal", { status: 403 })
  );
  const second = refusalFetch(
    async () => new Response("gateway preamble shared by every refusal", { status: 403 })
  );
  const later = refusalFetch(async () => new Response("Forbidden", { status: 403 }));
  const firstResponse = await first("https://idp.example/one", {
    headers: { authorization: `Bearer ${firstSecret}` },
  });
  await second("https://idp.example/two", { headers: { authorization: `Bearer ${secondSecret}` } });
  await later("https://idp.example/three", {});
  const error = new LibraryError(
    Object.assign(new Error(`Forbidden: ${firstSecret}; unrelated ${secondSecret}`), {
      response: firstResponse,
    })
  );
  expect(error.message.includes(firstSecret), `first request leaked: ${error.message}`).toBe(false);
  expect(error.message).toContain(secondSecret);
});
