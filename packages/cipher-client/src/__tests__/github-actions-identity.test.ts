// packages/cipher-client/src/__tests__/github-actions-identity.test.ts
//
// A GitHub Actions run's own identity, against a stub GitHub that answers what a real run was
// answered (`packages/envoy/internal/cipherclient/testdata/github-actions-token.json`, which the
// Go client's `githubactions_test.go` reads too), in the rows of that Go test: the job asks
// GitHub for its OIDC token for the broker's audience with the request token GitHub gave it, asks
// again for every trade, and trades the token GitHub answered. The request token travels only to
// GitHub, over https or to this machine's loopback, and never to a place a redirect names; no
// refusal prints it, nor anything GitHub answered.

import { afterEach, expect, test } from "bun:test";
import { randomUUID } from "node:crypto";
import { readFileSync } from "node:fs";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { generateKeyPair, SignJWT } from "jose";
import { newDpopKey } from "../dpop";
import {
  ACTIONS_ID_TOKEN_REQUEST_URL_VAR,
  githubActionsIdentity,
} from "../github-actions-identity";
import { BrokerTokenClient } from "../token-client";
import { SUBJECT_TOKEN_TYPE_GITHUB } from "../wire";
import { accepted, refusalOf, stubBroker } from "./stub";

/** GitHub's answers as a real run captured them. */
interface ActionsFixture {
  readonly request_url: { readonly query_keys: readonly string[] };
  readonly audience: string;
  readonly answer: {
    readonly status: number;
    readonly content_type: string;
    readonly body: Readonly<Record<string, string>>;
  };
  readonly jwt: {
    readonly header: Readonly<Record<string, string>> & { readonly alg: string };
    readonly claims: Readonly<Record<string, unknown>>;
  };
  readonly refused: {
    readonly request_token: string;
    readonly status: number;
    readonly content_type: string;
    readonly body: Readonly<Record<string, unknown>>;
  };
}

const FIXTURE = JSON.parse(
  readFileSync(
    new URL(
      "../../../envoy/internal/cipherclient/testdata/github-actions-token.json",
      import.meta.url
    ),
    "utf8"
  )
) as ActionsFixture;

/** The request token GitHub gave the job in these rows. */
const REQUEST_TOKEN = fixtureSecret("request");

/** Where the fixture's answer holds the token GitHub answered, which each suite signs itself. */
// biome-ignore lint/suspicious/noTemplateCurlyInString: the fixture writes its placeholder as `${jwt}`, and this is that file's own text rather than a template this suite writes.
const JWT_PLACEHOLDER = "${jwt}";

/** What every refusal of a read starts with. */
const ASKING = `ask GitHub for this run's OIDC token at the URL ${ACTIONS_ID_TOKEN_REQUEST_URL_VAR} names`;

const stops: (() => void)[] = [];
afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** The one RSA key every run token of this file is signed with, as GitHub signs with RS256. */
let runTokenKey: Promise<CryptoKey> | undefined;

/** A token GitHub could have answered: the fixture's header and claims, with a jti of its own. */
async function signRunToken(): Promise<string> {
  runTokenKey ??= generateKeyPair(FIXTURE.jwt.header.alg).then(({ privateKey }) => privateKey);
  return await new SignJWT({ ...FIXTURE.jwt.claims, jti: randomUUID() })
    .setProtectedHeader(FIXTURE.jwt.header)
    .sign(await runTokenKey);
}

/** The query GitHub writes into the request URL: the fixture's keys, each with a value of this
 *  test's own. */
function actionsQuery(): URLSearchParams {
  return new URLSearchParams(
    FIXTURE.request_url.query_keys.map((key): [string, string] => [key, `example-${key}`])
  );
}

/** One request the stub GitHub was sent, as it saw it. */
interface Asked {
  readonly method: string;
  readonly path: string;
  readonly query: readonly (readonly [string, string])[];
  readonly authorization: string | null;
}

/**
 * GitHub's OIDC token endpoint for one job: to {@link REQUEST_TOKEN} it answers the fixture's
 * answer with a newly signed token in its placeholder's place, as GitHub answers every request
 * with a new one, and to any other token the fixture's refusal. It keeps every request and every
 * token it answered.
 */
function stubGitHub(): {
  readonly requestUrl: string;
  readonly asked: Asked[];
  readonly answered: string[];
} {
  const asked: Asked[] = [];
  const answered: string[] = [];
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: async (request) => {
      const url = new URL(request.url);
      asked.push({
        method: request.method,
        path: url.pathname,
        query: [...url.searchParams],
        authorization: request.headers.get("Authorization"),
      });
      if (request.headers.get("Authorization") !== `bearer ${REQUEST_TOKEN}`) {
        return Response.json(FIXTURE.refused.body, { status: FIXTURE.refused.status });
      }
      const token = await signRunToken();
      answered.push(token);
      const body = Object.fromEntries(
        Object.entries(FIXTURE.answer.body).map(([member, value]) => [
          member,
          value === JWT_PLACEHOLDER ? token : value,
        ])
      );
      return Response.json(body, { status: FIXTURE.answer.status });
    },
  });
  stops.push(() => server.stop(true));
  return {
    requestUrl: `http://127.0.0.1:${server.port}/idtoken?${actionsQuery()}`,
    asked,
    answered,
  };
}

/** A server that answers every request with `answer`, counting them. */
function stubServer(answer: () => Response): {
  readonly url: string;
  readonly asked: () => number;
} {
  let asked = 0;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    fetch: () => {
      asked += 1;
      return answer();
    },
  });
  stops.push(() => server.stop(true));
  return { url: `http://127.0.0.1:${server.port}`, asked: () => asked };
}

/** The identity of a job GitHub gave `requestToken`, asking at `requestUrl`. */
function actionsIdentity(
  requestUrl: string,
  requestToken = REQUEST_TOKEN,
  fetchImpl?: typeof fetch
) {
  return githubActionsIdentity({
    requestUrl,
    requestToken,
    audience: FIXTURE.audience,
    fetch: fetchImpl,
  });
}

test("asks GitHub for the audience with the request token and trades the value", async () => {
  const github = stubGitHub();
  const broker = stubBroker(() => ({ body: accepted("dispatch-token-for-the-run", 900) }));
  stops.push(broker.stop);
  const client = new BrokerTokenClient({
    brokerUrl: broker.url,
    key: await newDpopKey(),
    subject: actionsIdentity(github.requestUrl),
    log: () => {},
  });
  stops.push(() => client.close());
  expect((await client.token()).accessToken).toBe("dispatch-token-for-the-run");
  const query = actionsQuery();
  query.set("audience", FIXTURE.audience);
  expect(github.asked).toEqual([
    {
      method: "GET",
      path: "/idtoken",
      // The request URL's own query, with the audience added to it.
      query: [...query],
      authorization: `bearer ${REQUEST_TOKEN}`,
    },
  ]);
  expect(github.answered).toHaveLength(1);
  expect(broker.trades).toHaveLength(1);
  expect(broker.trades[0]?.form.get("subject_token")).toBe(github.answered[0] as string);
  expect(broker.trades[0]?.form.get("subject_token_type")).toBe(SUBJECT_TOKEN_TYPE_GITHUB);
});

// The broker takes a GitHub token once, so every read is a token GitHub has just answered.
test("asks GitHub again for every trade", async () => {
  const github = stubGitHub();
  const identity = actionsIdentity(github.requestUrl);
  const read = [await identity.read(), await identity.read()];
  expect(github.asked).toHaveLength(2);
  expect(read).toEqual(
    github.answered.map((token) => ({ token, tokenType: SUBJECT_TOKEN_TYPE_GITHUB }))
  );
  expect(read[0]?.token).not.toBe(read[1]?.token);
});

// GitHub's refusal of a wrong request token, as a real run was answered: the error names the
// request URL by its variable and GitHub's status, and holds neither the request token nor any
// text of the answer. A request that got no answer names its class, and not the URL.
test("a refusal names its status and nothing the answer said", async () => {
  const github = stubGitHub();
  const wrong = `${FIXTURE.refused.request_token}-${fixtureSecret("wrong")}`;
  const refusal = await refusalOf(() => actionsIdentity(github.requestUrl, wrong).read());
  expect(refusal?.message).toBe(`${ASKING}: HTTP ${FIXTURE.refused.status}`);
  const printed = Bun.inspect(refusal);
  expect(printed).not.toContain(wrong);
  for (const said of Object.values(FIXTURE.refused.body)) {
    if (typeof said === "string") expect(printed).not.toContain(said);
  }
  expect(github.asked).toHaveLength(1);

  const secret = fixtureSecret("query");
  const lost = await refusalOf(() =>
    actionsIdentity(`http://127.0.0.1:1/idtoken?sig=${secret}`).read()
  );
  expect(lost?.message).toBe(`${ASKING}: no answer: ECONNREFUSED`);
  expect(lost?.cause).toBeUndefined();
  expect(Bun.inspect(lost)).not.toContain(secret);
});

// The request carries the job's request token, so an answer that names another place to ask is
// the refusal it is, and that place is asked nothing.
test.each([301, 302, 303, 307, 308])("follows no redirect: %p", async (status) => {
  const elsewhere = stubGitHub();
  const moving = stubServer(
    () => new Response(null, { status, headers: { Location: elsewhere.requestUrl } })
  );
  const refusal = await refusalOf(() =>
    actionsIdentity(`${moving.url}/idtoken?${actionsQuery()}`).read()
  );
  expect(refusal?.message).toBe(`${ASKING}: HTTP ${status}`);
  expect(moving.asked()).toBe(1);
  expect(elsewhere.asked).toHaveLength(0);
});

// The request token would travel in the clear to a plain-http URL, so a request URL that is not
// https is refused before anything is sent, unless it is on this machine's loopback, as a stub
// GitHub is. The refusal names the variable, and none of its value.
test("refuses a plain-http request URL off loopback", async () => {
  const secret = fixtureSecret("url");
  const sent: string[] = [];
  const recording = (async (input: string | URL | Request): Promise<Response> => {
    sent.push(String(input));
    throw new Error("no request is sent");
  }) as typeof fetch;
  for (const requestUrl of [
    `http://token.actions.example/idtoken?api-version=${secret}`,
    "http://127.0.0.1.example/idtoken?api-version=2.0",
    "http://localhost.example/idtoken?api-version=2.0",
    "ftp://token.actions.example/idtoken?api-version=2.0",
    `token.actions.example/idtoken?api-version=${secret}`,
    `http://[::1/idtoken?api-version=${secret}`,
  ]) {
    const refusal = await refusalOf(() =>
      actionsIdentity(requestUrl, REQUEST_TOKEN, recording).read()
    );
    expect(refusal?.message).toBe(
      `${ACTIONS_ID_TOKEN_REQUEST_URL_VAR} is not an https URL: the request token is sent there, so it takes https, or http to 127.0.0.1, ::1 or localhost`
    );
    expect(Bun.inspect(refusal)).not.toContain(secret);
  }
  expect(sent).toEqual([]);
  // Control: plain http to this machine's loopback is asked.
  const github = stubGitHub();
  await actionsIdentity(github.requestUrl).read();
  expect(github.asked).toHaveLength(1);
});

// A 2xx that holds no token is a refusal before the broker is asked, and an answer that is not
// JSON is named without its text, which can be a token.
test("refuses an answer with no value", async () => {
  const secret = fixtureSecret("answer");
  for (const [body, says] of [
    ["{}", "the answer holds no value"],
    ['{"value":""}', "the answer holds no value"],
    [secret, `HTTP ${FIXTURE.answer.status}: the answer is not JSON`],
  ] as const) {
    const github = stubServer(
      () =>
        new Response(body, {
          status: FIXTURE.answer.status,
          headers: { "Content-Type": FIXTURE.answer.content_type },
        })
    );
    const refusal = await refusalOf(() =>
      actionsIdentity(`${github.url}/idtoken?${actionsQuery()}`).read()
    );
    expect(refusal?.message).toBe(`${ASKING}: ${says}`);
    expect(Bun.inspect(refusal)).not.toContain(secret);
  }
});
