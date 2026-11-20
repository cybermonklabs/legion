import { afterAll, beforeEach, describe, expect, test } from "bun:test";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { dispatchBytes, dispatchCall, dispatchRefusal } from "./dispatch-driver-calls";

// A gateway or a proxy in Dispatch's place can answer a request with a page of its own that
// echoes the request back, its bearer or its session cookie with it. Every answer the drivers
// name that is not Dispatch's own error JSON is named in one line with the credential the
// request carried redacted (`bodyForText`), and Dispatch's own error JSON as Dispatch wrote it.

const token = fixtureSecret("agent");
const cookieValue = fixtureSecret("cookie");
const bearer = { Authorization: `Bearer ${token}` };
const person = { Cookie: `dsession=${cookieValue}`, Origin: "http://127.0.0.1" };

/** The page a gateway answers in Dispatch's place, echoing the request: each credential it
 *  carried bare on a line of its own, first, so only the redaction of the very credential the
 *  request sent can hide it, then its request line and headers as they came. */
function echoPage(request: Request, status = 502): Response {
  const authorization = request.headers.get("authorization") ?? "";
  const cookie = request.headers.get("cookie") ?? "";
  const bare = [authorization.replace(/^Bearer /, ""), cookie.slice(cookie.indexOf("=") + 1)];
  return new Response(
    `<html><head><title>${status} Bad Gateway</title></head><body>\n` +
      `${bare.filter((value) => value !== "").join("\n")}\n` +
      `<pre>${request.method} ${new URL(request.url).pathname} HTTP/1.1\n` +
      `authorization: ${authorization}\ncookie: ${cookie}</pre></body></html>\n`,
    { headers: { "Content-Type": "text/html" }, status }
  );
}

/** What the stand-in answers each request with; each test sets its own. */
let answer: (request: Request) => Response | Promise<Response> = echoPage;

const dispatch = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  fetch: (request) => answer(request),
});

// The drivers subscribe to the run's NATS before anything else, so their rows need one: this
// stand-in speaks as much of the protocol as a connect, a subscribe and a flush ask, its INFO and a
// PONG for each PING, and takes everything else in silence.
const nats = Bun.listen({
  hostname: "127.0.0.1",
  port: 0,
  socket: {
    open(socket) {
      const info = { headers: true, max_payload: 1_048_576, proto: 1, server_id: "stand-in" };
      socket.write(`INFO ${JSON.stringify({ ...info, version: "2.12.0" })}\r\n`);
    },
    data(socket, data) {
      for (const _ of new TextDecoder().decode(data).matchAll(/PING\r\n/g))
        socket.write("PONG\r\n");
    },
  },
});

afterAll(() => {
  dispatch.stop(true);
  nats.stop(true);
});

beforeEach(() => {
  answer = echoPage;
});

const dispatchUrl = `http://127.0.0.1:${dispatch.port}`;

/** The rejection `promise` ends in, as text. */
async function rejection(promise: Promise<unknown>): Promise<string> {
  try {
    await promise;
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
  throw new Error("the call answered, where a refusal was expected");
}

/** Holds that `text` names a body that is not Dispatch's error JSON and carries no credential of
 *  either kind. */
function expectNamed(text: string): void {
  expect(text).not.toContain(token);
  expect(text).not.toContain(cookieValue);
  expect(text).toContain("a body that is not Dispatch's error JSON (");
}

/** `expectNamed`, in one line: what an error message or a field holds. */
function expectNamedLine(text: string): void {
  expectNamed(text);
  expect(text).not.toContain("\n");
}

describe("dispatchCall", () => {
  test("names a proxy's page echoing the bearer in one line, the bearer redacted", async () => {
    expectNamedLine(
      await rejection(dispatchCall(dispatchUrl, "GET", "/api/v1/issues/CLM-1", bearer))
    );
  });

  test("names a proxy's page echoing the session cookie in one line, the cookie redacted", async () => {
    const message = await rejection(
      dispatchCall(dispatchUrl, "POST", "/api/v1/projects", person, { key: "CLM" })
    );
    expectNamedLine(message);
    expect(message).toStartWith("POST /api/v1/projects: 502 ");
  });

  test("prints Dispatch's own error JSON as Dispatch wrote it", async () => {
    const refusal = { code: "ISSUE_NOT_FOUND", error: "no issue carries the key CLM-9" };
    answer = () => Response.json(refusal, { status: 404 });
    expect(await rejection(dispatchCall(dispatchUrl, "GET", "/api/v1/issues/CLM-9", bearer))).toBe(
      `GET /api/v1/issues/CLM-9: 404 ${JSON.stringify(refusal)}`
    );
  });
});

describe("dispatchRefusal", () => {
  const write = { check: "no-such-check" };

  test("names a proxy's page in its answer and its words, the bearer redacted", async () => {
    const refused = await dispatchRefusal(dispatchUrl, "POST", "/x/results", bearer, write);
    expectNamedLine(refused.answer);
    expect(refused.answer).toStartWith("502 ");
    expectNamedLine(refused.error);
  });

  test("names JSON that is not Dispatch's error JSON in its words, the bearer redacted", async () => {
    // A gateway's JSON of its own, the bearer bare in a field where no Authorization or Bearer
    // pattern can find it, and the header as it came.
    answer = (request) => {
      const authorization = request.headers.get("authorization") ?? "";
      const echoed = { token: authorization.replace(/^Bearer /, ""), you_sent: authorization };
      return Response.json({ ...echoed, message: "upstream timed out" }, { status: 504 });
    };
    const refused = await dispatchRefusal(dispatchUrl, "POST", "/x/results", bearer, write);
    expectNamedLine(refused.error);
  });

  test("names a 2xx page that is not Dispatch's JSON in its words, the bearer redacted", async () => {
    answer = (request) => echoPage(request, 200);
    const refused = await dispatchRefusal(dispatchUrl, "POST", "/x/results", bearer, write);
    expect(refused.answer).toBe("200 (the write was taken)");
    expect(refused.error).not.toContain(token);
    expect(refused.error).toContain("a body that is not Dispatch's JSON (");
  });

  test("reads Dispatch's own refusal as Dispatch wrote it", async () => {
    answer = () =>
      Response.json(
        { code: "UNKNOWN_CHECK", error: "VRF-1 has no check no-such-check" },
        { status: 400 }
      );
    expect(await dispatchRefusal(dispatchUrl, "POST", "/x/results", bearer, write)).toEqual({
      answer: "400 UNKNOWN_CHECK",
      error: "VRF-1 has no check no-such-check",
    });
  });
});

describe("dispatchBytes", () => {
  test("names a proxy's page echoing the bearer in one line, the bearer redacted", async () => {
    expectNamedLine(await rejection(dispatchBytes(dispatchUrl, "/x/versions/1", bearer)));
  });
});

/** One run of `script` against the stand-ins, with the agent token in `tokenVariable` and the
 *  stand-in Dispatch as every URL it takes after the NATS one: its exit code and all it printed. */
async function drive(
  script: string,
  tokenVariable: string
): Promise<{ code: number; printed: string }> {
  const child = Bun.spawn(
    [
      "bun",
      join(import.meta.dir, "..", script),
      dispatchUrl,
      `nats://127.0.0.1:${nats.port}`,
      dispatchUrl,
    ],
    {
      env: {
        ...process.env,
        [tokenVariable]: token,
        VERIFICATION_RESULTS_E2E_DATABASE_URL: "postgres://stand-in.invalid/unused",
      },
      stderr: "pipe",
      stdout: "pipe",
    }
  );
  const [code, stdout, stderr] = await Promise.all([
    child.exited,
    new Response(child.stdout).text(),
    new Response(child.stderr).text(),
  ]);
  return { code, printed: `${stdout}${stderr}` };
}

/** The stand-in Dispatch for a driver's run: the dev sign-in's cookie, a 2xx for each fixture
 *  seed and each `ok` route (its JSON from the table), and a proxy's page for the first request
 *  `echoes` picks. */
function standIn(
  echoes: (request: Request, path: string) => boolean,
  ok: Record<string, object> = {}
): (request: Request) => Response {
  return (request) => {
    const path = new URL(request.url).pathname;
    if (path === "/auth/_dev/signin") {
      return new Response(null, {
        headers: { Location: "/", "Set-Cookie": `dsession=${cookieValue}; Path=/; HttpOnly` },
        status: 302,
      });
    }
    if (path.startsWith("/__fixture/")) return Response.json({});
    if (echoes(request, path)) return echoPage(request);
    return Response.json(ok[`${request.method} ${path}`] ?? {}, { status: 201 });
  };
}

describe("the drivers name a proxy's page in their failures", () => {
  const claimant = "dispatch-claimant-delivery.ts";
  const results = "verification-results.ts";

  test("dispatch-claimant-delivery's setup, under the person's cookie", async () => {
    answer = standIn((_, path) => path === "/api/v1/projects");
    const run = await drive(claimant, "CLAIMANT_E2E_AGENT_TOKEN");
    expect(run.printed).toContain("setup project failed: POST /api/v1/projects: 502 ");
    expectNamed(run.printed);
    expect(run.code).toBe(2);
  });

  test("dispatch-claimant-delivery's setup, under the agent token", async () => {
    answer = standIn((_, path) => path.endsWith("/claim"), {
      "POST /api/v1/issues": { key: "CLM-1" },
    });
    const run = await drive(claimant, "CLAIMANT_E2E_AGENT_TOKEN");
    expect(run.printed).toContain("setup claim of CLM-1 by claimant-e2e-p failed: POST ");
    expectNamed(run.printed);
    expect(run.code).toBe(2);
  });

  test("verification-results' setup, under the person's cookie", async () => {
    answer = standIn((_, path) => path === "/api/v1/projects");
    const run = await drive(results, "VERIFICATION_RESULTS_E2E_AGENT_TOKEN");
    expect(run.printed).toContain("setup project VRF failed: POST /api/v1/projects: 502 ");
    expectNamed(run.printed);
    expect(run.code).toBe(2);
  });

  test("verification-results' setup, under the agent token", async () => {
    answer = standIn((_, path) => path.startsWith("/api/v1/settings/repo-projects/"));
    const run = await drive(results, "VERIFICATION_RESULTS_E2E_AGENT_TOKEN");
    expect(run.printed).toContain("setup repository mapping acme/verification-live failed: PUT ");
    expectNamed(run.printed);
    expect(run.code).toBe(2);
  });

  test("verification-results' first case, a refusal under the agent token", async () => {
    answer = standIn((_, path) => path.endsWith("/results"), {
      "POST /api/v1/issues": { key: "VRF-1" },
    });
    const run = await drive(results, "VERIFICATION_RESULTS_E2E_AGENT_TOKEN");
    expect(run.printed).toContain(
      "FAIL the-results-route-is-served: the record route answers 502 "
    );
    expectNamed(run.printed);
    expect(run.code).toBe(1);
  });
});
