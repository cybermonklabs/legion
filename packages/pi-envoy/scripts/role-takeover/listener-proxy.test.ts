import { afterEach, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import { join } from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectories } from "@legion/test-fixtures/temporary-directories";

// The recording proxy of scripts/smoke-role-takeover.sh, run as the smoke runs it, with a gateway
// in the listener's place that answers with its own page echoing the request back, as a
// misconfigured proxy does. The client's credential travels through this proxy in the
// `Authorization` header it forwards, so the answer it logs must never hold that credential; the
// listener's own JSON, whose fields the smoke's readers take (`.response.holder`,
// `.response.deleted` and `.response.reason` in role-takeover/clients.sh), is logged as it came.

const PROXY = join(import.meta.dir, "listener-proxy.ts");
const scratch = temporaryDirectories("listener-proxy");
const stops: (() => void)[] = [];

afterEach(() => {
  for (const stop of stops.splice(0)) stop();
});

/** A server in the listener's place on a loopback port of its own, answering with `answer`. */
function upstream(answer: (request: Request, secret: string) => Response) {
  const carried: string[] = [];
  const server = Bun.serve({
    fetch(request) {
      const authorization = request.headers.get("authorization") ?? "";
      carried.push(authorization);
      return answer(request, authorization.replace(/^\S+\s+/, ""));
    },
    hostname: "127.0.0.1",
    port: 0,
  });
  stops.push(() => server.stop(true));
  return { carried, url: `http://127.0.0.1:${server.port}` };
}

/** The gateway's page: `secret` bare on a line of its own, then the request echoed back. */
function echoPage(request: Request, secret: string): string {
  return [
    "<html><head><title>502 Bad Gateway</title></head><body>",
    secret,
    `<pre>${request.method} ${new URL(request.url).pathname} HTTP/1.1`,
    ...[...request.headers].map(([name, value]) => `${name}: ${value}`),
    "</pre></body></html>",
  ].join("\n");
}

/** The proxy in front of `upstreamUrl`, logging to a file of its own, once it is listening. */
async function startProxy(upstreamUrl: string) {
  const log = join(scratch(), "proxy.jsonl");
  const child = Bun.spawn(
    [
      process.execPath,
      PROXY,
      "--port",
      "0",
      "--upstream",
      upstreamUrl,
      "--log",
      log,
      "--label",
      "echo",
    ],
    { stderr: "pipe", stdout: "pipe" }
  );
  stops.push(() => child.kill("SIGKILL"));
  const reader = child.stdout.getReader();
  const decoder = new TextDecoder();
  let printed = "";
  while (!/ready on \d+\n/.test(printed)) {
    const chunk = await reader.read();
    if (chunk.done) throw new Error(`the proxy ended before it was ready: ${printed}`);
    printed += decoder.decode(chunk.value, { stream: true });
  }
  return { log, url: `http://127.0.0.1:${/ready on (\d+)\n/.exec(printed)?.[1]}` };
}

/** Each line the proxy logged, parsed. */
function logged(log: string): { status: number; response: unknown }[] {
  return readFileSync(log, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line) as { status: number; response: unknown });
}

/** One claim through the proxy under `authorization`, as a client sends it. It goes through Bun's
 *  own fetch: in a run of the whole package, extensions/envoy.test.ts has put a recording getter
 *  on `globalThis.fetch` for the process, which answers with the last fetch one of its rows set. */
async function claim(proxyUrl: string, authorization: string): Promise<void> {
  const answer = await Bun.fetch(`${proxyUrl}/v1/roles/set`, {
    body: JSON.stringify({ role: "controller", session_id: "sess-b" }),
    headers: { Authorization: authorization, "Content-Type": "application/json" },
    method: "POST",
  });
  await answer.text();
}

test("an answer a gateway writes is logged through the excerpt, never the client's bearer", async () => {
  const token = fixtureSecret("client");
  const listener = upstream(
    (request, secret) =>
      new Response(echoPage(request, secret), {
        headers: { "Content-Type": "text/html" },
        status: 502,
      })
  );
  const proxy = await startProxy(listener.url);
  await claim(proxy.url, `Bearer ${token}`);
  expect(listener.carried).toEqual([`Bearer ${token}`]);
  expect(readFileSync(proxy.log, "utf8")).not.toContain(token);
  const [entry] = logged(proxy.log);
  expect(entry?.status).toBe(502);
  expect(entry?.response).toStartWith(`a body that is not the listener's JSON ("`);
});

test("JSON a gateway writes that echoes the client's credential is logged through the excerpt, never the credential", async () => {
  const token = fixtureSecret("client");
  const listener = upstream((request, secret) =>
    Response.json({ echoed: secret, headers: Object.fromEntries(request.headers) }, { status: 200 })
  );
  const proxy = await startProxy(listener.url);
  await claim(proxy.url, `DPoP ${token}`);
  expect(listener.carried).toEqual([`DPoP ${token}`]);
  expect(readFileSync(proxy.log, "utf8")).not.toContain(token);
  const [entry] = logged(proxy.log);
  expect(entry?.status).toBe(200);
  expect(entry?.response).toStartWith(`a body that is not the listener's JSON ("`);
});

test("the listener's own JSON is logged as it came, for the smoke's readers", async () => {
  const token = fixtureSecret("client");
  const refusal = { error: "the role is held", holder: "sess-a" };
  const listener = upstream(() => Response.json(refusal, { status: 409 }));
  const proxy = await startProxy(listener.url);
  await claim(proxy.url, `Bearer ${token}`);
  expect(logged(proxy.log).map(({ status, response }) => ({ response, status }))).toEqual([
    { response: refusal, status: 409 },
  ]);
});
