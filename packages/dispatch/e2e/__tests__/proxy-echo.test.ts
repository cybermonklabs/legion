import { afterEach, expect, test } from "bun:test";
import { mkdtempSync, rmSync, writeFileSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";

import snapshot from "../../../contracts/fixtures/live/snapshot.json";
import type {
  DeliveryLiveSnapshot,
  DeliveryLiveView,
  DeliveryTimelineWithMeasures,
  LiveClientFrame,
} from "../../web/src/api/types";
import { listAgents } from "../api";
import type { ProbeCompareLine, ProbeLine } from "../delivery-live-probe";
import { requireOneCookieKey } from "../global-setup";
import { rowLimit, runBounded } from "./bounded-subprocess";

// A proxy or a gateway in Dispatch's place can answer a request with a page of its own that echoes
// the request back, headers and all, so a client that prints such a body prints the credential
// the request carried. Each row here puts that gateway in front of one client of this directory
// and reads what the client printed: never the credential, and the body named through the
// excerpt (`bodyForText`, packages/envoy-client/src/excerpt.ts). Every gateway also writes the
// credential bare on a line of its own near the top of its page, where no `Authorization:` or
// `Bearer` stands before it and the excerpt's 120 characters still reach it, so only the
// redaction of that very credential can hide it. Each row also reads that the request did carry
// the credential, so its absence from what was printed is the redaction's doing.

const dispatchPackage = fileURLToPath(new URL("../..", import.meta.url));
const api = fileURLToPath(new URL("../api.ts", import.meta.url));
const probe = fileURLToPath(new URL("../delivery-live-probe.ts", import.meta.url));

const stops: (() => void)[] = [];
const scratchDirs: string[] = [];

afterEach(() => {
  for (const stop of stops.splice(0)) stop();
  for (const dir of scratchDirs.splice(0)) rmSync(dir, { force: true, recursive: true });
});

/** A gateway's page as a misconfigured proxy writes it: each of `credentials` bare on a line of
 *  its own, then the request it answers echoed back, its request line and every header. */
function echoPage(request: Request, credentials: readonly string[]): string {
  const url = new URL(request.url);
  return [
    "<html><head><title>502 Bad Gateway</title><style>pre { color: grey }</style></head><body>",
    ...credentials,
    `<pre>${request.method} ${url.pathname}${url.search} HTTP/1.1`,
    ...[...request.headers].map(([name, value]) => `${name}: ${value}`),
    "</pre></body></html>",
  ].join("\n");
}

/** A server on a loopback port of its own that answers every request with `answer`, and the
 *  `Authorization` and `Cookie` headers each request carried, by path. */
function gateway(answer: (request: Request) => Response) {
  const carried: { path: string; authorization: string; cookie: string }[] = [];
  const server = Bun.serve({
    fetch(request) {
      carried.push({
        authorization: request.headers.get("authorization") ?? "",
        cookie: request.headers.get("cookie") ?? "",
        path: new URL(request.url).pathname,
      });
      return answer(request);
    },
    hostname: "127.0.0.1",
    port: 0,
  });
  stops.push(() => server.stop(true));
  return { carried, url: `http://127.0.0.1:${server.port}` };
}

/** The gateway's page as its 502 answer. */
function badGateway(request: Request, credentials: readonly string[]): Response {
  return new Response(echoPage(request, credentials), {
    headers: { "Content-Type": "text/html" },
    status: 502,
  });
}

/** What `call` rejected with, as its message; a call that resolves fails the row. */
async function refusalOf(call: Promise<unknown>): Promise<string> {
  try {
    await call;
  } catch (error) {
    return error instanceof Error ? error.message : String(error);
  }
  throw new Error("the call resolved, and the row wants its refusal");
}

const NOT_DISPATCH = `a body that is not Dispatch's error JSON ("`;

test("a bearer call of e2e/api.ts that a gateway answers names its body through the excerpt, never the bearer", async () => {
  const token = fixtureSecret("agent");
  const dispatch = gateway((request) => badGateway(request, [token]));
  const refusal = await refusalOf(listAgents({ as: "agent", baseUrl: dispatch.url, token }));
  expect(dispatch.carried.map(({ authorization }) => authorization)).toEqual([`Bearer ${token}`]);
  expect(refusal).not.toContain(token);
  expect(refusal).toStartWith(`GET /api/v1/agents failed: 502 ${NOT_DISPATCH}`);
});

test("a call of e2e/api.ts that Dispatch itself refuses names Dispatch's error JSON as it came", async () => {
  const token = fixtureSecret("agent");
  const refused = JSON.stringify({
    code: "OAUTH_SCOPE_REQUIRED",
    error: "the token holds no scope",
  });
  const dispatch = gateway(
    () => new Response(refused, { headers: { "Content-Type": "application/json" }, status: 403 })
  );
  const refusal = await refusalOf(listAgents({ as: "agent", baseUrl: dispatch.url, token }));
  expect(refusal).toBe(`GET /api/v1/agents failed: 403 ${refused}`);
});

/** A dev sign-in in Dispatch's place: a 302 that sets `cookie` as the session cookie. */
function signedIn(cookie: string): Response {
  return new Response(null, {
    headers: {
      Location: "/",
      "Set-Cookie": `dsession=${cookie}; Path=/; HttpOnly; SameSite=Strict; Max-Age=2592000`,
    },
    status: 302,
  });
}

// `e2e/api.ts` reads the server it signs a person in at when it is imported (`baseUrl`), so the
// call runs in a process of its own, pointed at the gateway by PLAYWRIGHT_BASE_URL as a deployed
// run is, and prints the refusal it got.
test(
  "a session-cookie call of e2e/api.ts that a gateway answers names its body through the excerpt, never the cookie",
  async () => {
    const cookie = fixtureSecret("session");
    const dispatch = gateway((request) =>
      new URL(request.url).pathname === "/auth/_dev/signin"
        ? signedIn(cookie)
        : badGateway(request, [cookie])
    );
    const script = [
      `import { listAgents } from ${JSON.stringify(api)};`,
      "try { await listAgents({ login: 'alice' }); console.log('no refusal'); }",
      "catch (error) { console.log(error instanceof Error ? error.message : String(error)); }",
    ].join("\n");
    const run = await runBounded({
      argv: [process.execPath, "-e", script],
      bound: "short",
      cwd: dispatchPackage,
      env: { ...process.env, E2E_AGENT_TOKEN: undefined, PLAYWRIGHT_BASE_URL: dispatch.url },
      from: import.meta.url,
    });
    expect(run.code, `${run.named}: ${run.stderr}`).toBe(0);
    expect(dispatch.carried.map(({ path, cookie: sent }) => [path, sent])).toEqual([
      ["/auth/_dev/signin", ""],
      ["/api/v1/agents", `dsession=${cookie}`],
    ]);
    expect(`${run.stdout}${run.stderr}`).not.toContain(cookie);
    expect(run.stdout).toStartWith(`GET /api/v1/agents failed: 502 ${NOT_DISPATCH}`);
  },
  rowLimit("short")
);

test("the harness's cookie-key check that a gateway answers names its body through the excerpt, never the cookie", async () => {
  const cookie = fixtureSecret("session");
  const dispatch = gateway((request) =>
    new URL(request.url).pathname === "/auth/_dev/signin"
      ? signedIn(cookie)
      : badGateway(request, [cookie])
  );
  const refusal = await refusalOf(requireOneCookieKey(dispatch.url));
  expect(dispatch.carried.map(({ path, cookie: sent }) => [path, sent])).toEqual([
    ["/auth/_dev/signin", ""],
    ["/auth/whoami", `dsession=${cookie}`],
  ]);
  expect(refusal).not.toContain(cookie);
  expect(refusal).toStartWith(
    `The Dispatch e2e harness cannot start: Dispatch B answered 502 ${NOT_DISPATCH}`
  );
  expect(refusal).toContain(") to a session Dispatch A minted");
});

// The probe, `e2e/delivery-live-probe.ts`, as an agent runs it: a process of its own, watching a
// timeline view for a moment and then comparing it with the two GET routes, the first of which
// the gateway answers. It writes the comparison as a JSON line, and a refused read carries the
// code and the message it names the answer by.

const view: DeliveryLiveView = {
  kind: "timeline",
  query: "repo=acme%2Fwidgets",
  window: { preset: 28 },
};
const held = snapshot as unknown as DeliveryLiveSnapshot;
const heldMeasures = (held.body as DeliveryTimelineWithMeasures).measures;

/** A Dispatch on a loopback port of its own for the probe: each subscribe gets the fixture
 *  snapshot, `GET /api/v1/delivery/timeline` gets `timeline`, the measures route the measures the
 *  snapshot holds, and each read's `Authorization` is kept. */
function probeDispatch(timeline: (request: Request) => Response) {
  const authorizations: string[] = [];
  const server = Bun.serve({
    fetch(request, srv) {
      const url = new URL(request.url);
      if (url.pathname === "/ws/live") {
        return srv.upgrade(request) ? undefined : new Response("upgrade failed", { status: 400 });
      }
      authorizations.push(request.headers.get("authorization") ?? "");
      return url.pathname === "/api/v1/delivery/timeline"
        ? timeline(request)
        : Response.json(heldMeasures);
    },
    hostname: "127.0.0.1",
    port: 0,
    websocket: {
      message(socket, raw) {
        // The probe's own frame, a subscribe.
        const frame = JSON.parse(String(raw)) as LiveClientFrame;
        if (frame.type === "subscribe") socket.send(JSON.stringify({ ...held, sub: frame.sub }));
      },
    },
  });
  stops.push(() => server.stop(true));
  return { authorizations, url: `http://127.0.0.1:${server.port}` };
}

/** The probe run to its end against `url` under `token`: a watch of 1.2 s from its open, then the
 *  comparison. */
async function probeRun(url: string, token: string) {
  const dir = mkdtempSync(join(tmpdir(), "proxy-echo-probe-"));
  scratchDirs.push(dir);
  const tokenFile = join(dir, "token");
  writeFileSync(tokenFile, `${token}\n`, { mode: 0o600 });
  const run = await runBounded({
    argv: [
      process.execPath,
      probe,
      "--url",
      url,
      "--token-file",
      tokenFile,
      "--view",
      JSON.stringify(view),
      "--minutes",
      "0.02",
      "--compare",
      "--read-within",
      "30",
    ],
    bound: "short",
    cwd: dispatchPackage,
    from: import.meta.url,
  });
  const lines = run.stdout
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as ProbeLine);
  const timeline = lines.find(
    (line): line is ProbeCompareLine =>
      line.type === "compare" && line.route.startsWith("/api/v1/delivery/timeline?")
  );
  return { ...run, timeline };
}

for (const [shape, status, body, code, notWhat] of [
  ["a page", 200, (page: string) => page, "NOT_JSON", "JSON"],
  ["a page", 502, (page: string) => page, "HTTP_502", "Dispatch's error JSON"],
  [
    "a JSON string",
    502,
    (page: string) => JSON.stringify(page),
    "HTTP_502",
    "Dispatch's error JSON",
  ],
  [
    "a JSON object with no error",
    502,
    (page: string) => JSON.stringify({ code: "BAD_GATEWAY", echo: page }),
    "HTTP_502",
    "Dispatch's error JSON",
  ],
] as const) {
  test(
    `a --compare read a gateway answers ${status} with ${shape} names its body through the excerpt, never the bearer`,
    async () => {
      const token = fixtureSecret("probe");
      const dispatch = probeDispatch(
        (request) => new Response(body(echoPage(request, [token])), { status })
      );
      const run = await probeRun(dispatch.url, token);
      expect(run.code, `${run.named}: ${run.stderr}`).toBe(1);
      expect(dispatch.authorizations).toContain(`Bearer ${token}`);
      expect(`${run.stdout}${run.stderr}`).not.toContain(token);
      expect(run.timeline?.error?.message).toStartWith(`a body that is not ${notWhat} ("`);
      expect(run.timeline?.error?.code).toBe(code);
    },
    rowLimit("short")
  );
}

test(
  "a --compare read Dispatch itself refuses carries Dispatch's own code and error",
  async () => {
    const token = fixtureSecret("probe");
    const dispatch = probeDispatch(() =>
      Response.json(
        { code: "OAUTH_SCOPE_REQUIRED", error: "the token holds no scope" },
        { status: 403 }
      )
    );
    const run = await probeRun(dispatch.url, token);
    expect(run.code, `${run.named}: ${run.stderr}`).toBe(1);
    expect(run.timeline?.error).toEqual({
      code: "OAUTH_SCOPE_REQUIRED",
      message: "the token holds no scope",
    });
  },
  rowLimit("short")
);
