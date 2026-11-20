import { afterAll, beforeAll, beforeEach, expect, test } from "bun:test";
import { writeFileSync } from "node:fs";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { Rig } from "./session-settings-common";
import { Services } from "./session-settings-services";

// The dispatch-session-settings driver's requests that carry a bearer - an agent's Send and accept
// with the session's Dispatch token, and every listener call with the Envoy token - against a
// gateway in the server's place that answers with a page of its own, echoing the request back.
// What the driver prints of each answer (`JSON.stringify(answer.body)`, and the listener's
// failure) names such a page in one line with the bearer redacted; the server's own error JSON is
// kept as it parsed, which is what the driver's checks read.

const dispatchToken = fixtureSecret("dispatch");
const envoyToken = fixtureSecret("envoy");
const work = temporaryDirectory("session-settings-services-test");

/** The page a gateway answers in the server's place: the bearer the request carried bare on a
 *  line of its own, first, so only the redaction of that very bearer can hide it, then the
 *  request's headers as they came. */
function echoPage(request: Request): Response {
  const authorization = request.headers.get("authorization") ?? "";
  return new Response(
    `<html><body>\n${authorization.replace(/^Bearer /, "")}\n<pre>${request.method} ` +
      `${new URL(request.url).pathname}\nauthorization: ${authorization}</pre></body></html>\n`,
    { headers: { "Content-Type": "text/html" }, status: 502 }
  );
}

let answer: (request: Request) => Response = echoPage;
const server = Bun.serve({ hostname: "127.0.0.1", port: 0, fetch: (request) => answer(request) });
afterAll(() => server.stop(true));
beforeEach(() => {
  answer = echoPage;
});

let services: Services;
beforeAll(() => {
  const dispatchTokenFile = join(work, "dispatch-token");
  const envoyTokenFile = join(work, "envoy-token");
  writeFileSync(dispatchTokenFile, `${dispatchToken}\n`, { mode: 0o600 });
  writeFileSync(envoyTokenFile, `${envoyToken}\n`, { mode: 0o600 });
  const url = `http://127.0.0.1:${server.port}`;
  // The four fields the requests read; the rest of a run's setup is nothing these calls touch.
  const rig = { dispatchTokenFile, dispatchUrl: url, envoyTokenFile, envoyUrl: url, work };
  services = new Services(rig as unknown as Rig);
});

/** Holds that `printed` names a page that is not `server`'s error JSON, with no bearer in it. */
function expectNamed(printed: string, server: string): void {
  expect(printed).not.toContain(dispatchToken);
  expect(printed).not.toContain(envoyToken);
  expect(printed).toContain(`a body that is not ${server}'s error JSON (`);
}

test("an agent's Send names a proxy's page in one line, the Dispatch bearer redacted", async () => {
  const sent = await services.sendAsAgent("ses_main", "/model b");
  expect(sent.status).toBe(502);
  expectNamed(JSON.stringify(sent.body), "Dispatch");
});

test("an agent's accept names a proxy's page in one line, the Dispatch bearer redacted", async () => {
  const accepted = await services.accept("message-1", 1, "ses_main");
  expect(accepted.status).toBe(502);
  expectNamed(JSON.stringify(accepted.body), "Dispatch");
});

test("Dispatch's own refusal is kept as it parsed, for the checks that read it", async () => {
  const refusal = { code: "OAUTH_SCOPE_REQUIRED", error: "an agent bearer cannot send this" };
  answer = () => Response.json(refusal, { status: 403 });
  expect(await services.sendAsAgent("ses_main", "/model b")).toEqual({
    body: refusal,
    status: 403,
  });
});

test("a listener call names a proxy's page in its failure, the Envoy bearer redacted", async () => {
  let failure = "";
  try {
    await services.registered("ses_main");
  } catch (error) {
    failure = error instanceof Error ? error.message : String(error);
  }
  expect(failure).toStartWith("the listener answered /v1/sessions with 502: ");
  expectNamed(failure, "the listener");
  expect(failure).not.toContain("\n");
});

test("the listener's own refusal prints as the listener wrote it", async () => {
  const refusal = { error: "session ses_main is not registered", reason: "not_found" };
  answer = () => Response.json(refusal, { status: 404 });
  await expect(services.registered("ses_main")).rejects.toThrow(
    `the listener answered /v1/sessions with 404: ${JSON.stringify(refusal)}`
  );
});
