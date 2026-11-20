// packages/broker-client/src/__tests__/github-actions.test.ts
//
// The identity of a GitHub Actions job: the OIDC token its runner mints for one audience, asked
// of the runner's own endpoint with the bearer the runner gave the job, and presented as a
// workflow run's token. A job's token file is the other way to hold one.

import { afterAll, expect, test } from "bun:test";
import { writeFile } from "node:fs/promises";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";
import {
  ACTIONS_TOKEN_URL_VAR,
  githubActionsIdentity,
  workflowRunTokenFile,
} from "../github-actions";
import { SUBJECT_TOKEN_TYPE_GITHUB } from "../wire";

const makeDirectory = temporaryDirectories("broker-client-github-actions");

const REQUEST_TOKEN = fixtureSecret("runner");

const servers: { stop: (closeActive?: boolean) => void }[] = [];
afterAll(() => {
  for (const server of servers) server.stop(true);
});

/** One request the stub runner endpoint was asked, as it saw it. */
interface RunnerRequest {
  readonly path: string;
  readonly query: Record<string, string>;
  readonly authorization: string | null;
}

/** A stand-in for the runner's OIDC token endpoint: each request gets `answer(n)`, and the
 *  endpoint keeps what it was asked. Its URL carries a query, as the runner's own does. */
function stubRunner(answer: (n: number) => Response): { url: string; asked: RunnerRequest[] } {
  const asked: RunnerRequest[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: (request) => {
      const url = new URL(request.url);
      asked.push({
        path: url.pathname,
        query: Object.fromEntries(url.searchParams),
        authorization: request.headers.get("Authorization"),
      });
      return answer(asked.length);
    },
  });
  servers.push(server);
  return { url: `http://127.0.0.1:${server.port}/_apis/idtoken?api-version=2.0`, asked };
}

test("a job's token is asked of the runner's endpoint with its bearer and the audience, at every read", async () => {
  // The run's OIDC token the runner mints at each ask, random as a real one is.
  const minted = [fixtureSecret("oidc"), fixtureSecret("oidc")];
  const runner = stubRunner((n) => Response.json({ count: 1, value: minted[n - 1] }));
  const source = githubActionsIdentity({
    requestUrl: runner.url,
    requestToken: REQUEST_TOKEN,
    audience: "example-audience",
  });

  expect(await source.read()).toEqual({
    token: minted[0],
    tokenType: SUBJECT_TOKEN_TYPE_GITHUB,
  });
  // The broker takes a run's token once, so each trade asks the runner for a new one.
  expect((await source.read()).token).toBe(minted[1]);
  const asked = {
    path: "/_apis/idtoken",
    query: { "api-version": "2.0", audience: "example-audience" },
    authorization: `Bearer ${REQUEST_TOKEN}`,
  };
  expect(runner.asked).toEqual([asked, asked]);
  expect(source.describe()).toContain(ACTIONS_TOKEN_URL_VAR);
  expect(source.describe()).not.toContain(REQUEST_TOKEN);
});

test("an endpoint that refuses, or answers no token, is an error naming the variable and never the bearer", async () => {
  for (const [answer, says] of [
    [() => new Response(`denied for ${REQUEST_TOKEN}`, { status: 403 }), "HTTP 403"],
    [() => Response.json({ count: 0 }), "no token"],
    [() => new Response("not json"), "no token"],
  ] as const) {
    const runner = stubRunner(answer);
    const source = githubActionsIdentity({
      requestUrl: runner.url,
      requestToken: REQUEST_TOKEN,
      audience: "example-audience",
    });
    const refusal = await source.read().then(
      () => null,
      (error: unknown) => error as Error
    );
    expect(refusal?.message, says).toContain(ACTIONS_TOKEN_URL_VAR);
    expect(refusal?.message, says).toContain(says);
    expect(refusal?.message, says).not.toContain(REQUEST_TOKEN);
  }
});

// The request carries the runner's bearer. A redirect it followed would send that bearer on to a
// target on the runner's own origin, and take a token that another origin minted as the job's.
// So no request follows one: a 3xx is the endpoint's refusal, named by its status.
test("a redirect of the runner's endpoint is followed by no request, and is refused naming its status", async () => {
  const elsewhere = stubRunner(() => Response.json({ value: "token-from-the-other-origin" }));
  for (const status of [301, 302, 303, 307, 308]) {
    for (const [target, sameOrigin] of [
      [elsewhere.url, false],
      ["/_apis/collect", true],
    ] as const) {
      const runner = stubRunner((n) =>
        n === 1
          ? new Response(`Redirecting to ${target}`, { status, headers: { Location: target } })
          : Response.json({ value: "token-of-the-redirect" })
      );
      const source = githubActionsIdentity({
        requestUrl: runner.url,
        requestToken: REQUEST_TOKEN,
        audience: "example-audience",
      });
      const refusal = await source.read().then(
        (token) => token,
        (error: unknown) => error as Error
      );
      const label = `${status} to ${sameOrigin ? "its own origin" : "another origin"}`;
      expect(refusal instanceof Error ? refusal.message : refusal, label).toBe(
        `the runner's OIDC token endpoint (${ACTIONS_TOKEN_URL_VAR}) answered HTTP ${status}`
      );
      expect(runner.asked.length, label).toBe(1);
    }
  }
  expect(elsewhere.asked).toEqual([]);
});

test("a runner URL that is no URL is an error naming the variable, and asks nothing", async () => {
  const asked: unknown[] = [];
  const source = githubActionsIdentity({
    requestUrl: "not a URL",
    requestToken: REQUEST_TOKEN,
    audience: "example-audience",
    fetchImpl: (async (input: string | URL | Request) => {
      asked.push(input);
      return Response.json({ value: "a.token" });
    }) as typeof fetch,
  });
  const refusal = await source.read().then(
    () => null,
    (error: unknown) => error as Error
  );
  expect(refusal?.message).toBe(
    `the runner's OIDC token endpoint (${ACTIONS_TOKEN_URL_VAR}) is no URL`
  );
  expect(asked).toEqual([]);
});

test("a job's token file is read as the workflow run's token at every read, and an empty one is refused", async () => {
  const path = join(makeDirectory(), "run-token");
  const first = fixtureSecret("run");
  const second = fixtureSecret("run");
  await writeFile(path, `${first}\n`, { mode: 0o600 });
  const source = workflowRunTokenFile(path);
  expect(await source.read()).toEqual({
    token: first,
    tokenType: SUBJECT_TOKEN_TYPE_GITHUB,
  });
  await writeFile(path, `${second}\n`);
  expect((await source.read()).token).toBe(second);
  expect(source.describe()).toContain(path);
  await writeFile(path, "  \n");
  expect(source.read()).rejects.toThrow(`${path} is empty`);
});
