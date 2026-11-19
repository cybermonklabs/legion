import { afterEach, expect, test } from "bun:test";
import { REDIRECT_CODES, type RedirectServers, redirectServers } from "./redirect-servers";

const started: RedirectServers[] = [];
afterEach(() => {
  for (const servers of started.splice(0)) servers.stop();
});

// The instrument of every redirect row: a client that follows the redirect must show up in
// `received`, or an empty `received` proves nothing.
test("a client that follows each redirect reaches the other server, which records what it sent", async () => {
  const servers = redirectServers();
  started.push(servers);
  for (const code of REDIRECT_CODES) {
    servers.code = code;
    const answer = await fetch(`${servers.origin}/v1/send?to=b`, {
      method: "POST",
      headers: { "x-api-key": `made-up-key-${code}` },
      body: "made-up body",
    });
    expect(answer.status).toBe(200);
  }
  expect(servers.elsewhere).not.toBe(servers.origin);
  expect(servers.received.map(({ method, path }) => `${method} ${path}`)).toEqual([
    "GET /v1/send?to=b",
    "GET /v1/send?to=b",
    "GET /v1/send?to=b",
    "POST /v1/send?to=b",
    "POST /v1/send?to=b",
  ]);
  expect(servers.received.map(({ headers }) => headers["x-api-key"])).toEqual(
    REDIRECT_CODES.map((code) => `made-up-key-${code}`)
  );
  expect(servers.received.at(-1)?.body).toBe("made-up body");
});

test("a client that follows no redirect gets the status and a Location on the other server, which receives nothing", async () => {
  const servers = redirectServers({
    direct: () => new Response("direct"),
    redirects: (path) => path === "/moved",
  });
  started.push(servers);
  for (const code of REDIRECT_CODES) {
    servers.code = code;
    const answer = await fetch(`${servers.origin}/moved?q=1`, { redirect: "manual" });
    expect(answer.status).toBe(code);
    expect(answer.headers.get("Location")).toBe(`${servers.elsewhere}/moved?q=1`);
  }
  expect(await (await fetch(`${servers.origin}/kept`)).text()).toBe("direct");
  expect(servers.received).toEqual([]);
});

test("a redirect names the Location a test sets, or none, as it is written", async () => {
  const servers = redirectServers();
  started.push(servers);
  for (const [location, named] of [
    ["/another-path?q=1", "/another-path?q=1"],
    ["", ""],
    ["http://[not a host", "http://[not a host"],
    [null, null],
  ] as const) {
    servers.location = location;
    const answer = await fetch(`${servers.origin}/asked`, { redirect: "manual" });
    expect(answer.headers.get("Location"), String(location)).toBe(named);
  }
  servers.location = undefined;
  const answer = await fetch(`${servers.origin}/asked`, { redirect: "manual" });
  expect(answer.headers.get("Location")).toBe(`${servers.elsewhere}/asked`);
});

/** What `pending` settled with by the next turn of the event loop, or "pending". */
async function byTheNextTurn(pending: Promise<unknown>): Promise<unknown> {
  const bound = Promise.withResolvers<"pending">();
  setImmediate(() => bound.resolve("pending"));
  return await Promise.race([pending, bound.promise]);
}

// A client that reads a redirect's body before its status waits on this body for good, so a row
// that gives it one shows whether the client refuses before it reads.
test("an endless body sends its first chunk and then nothing, until the server stops", async () => {
  const servers = redirectServers();
  started.push(servers);
  servers.endlessBody = true;
  const answer = await fetch(`${servers.origin}/asked`, { redirect: "manual" });
  expect(answer.status).toBe(307);
  const reader = answer.body?.getReader();
  if (reader === undefined) throw new Error("the redirect had no body");
  const first = await reader.read();
  expect(new TextDecoder().decode(first.value)).toBe("the first chunk of a body that ");
  const next = reader.read().then(
    (read) => (read.done ? "ended" : "more"),
    () => "failed"
  );
  expect(await byTheNextTurn(next)).toBe("pending");
  servers.stop();
  expect(await next).not.toBe("more");
  // The control: with no endless body, the redirect's body is empty and ends at once.
  const plain = redirectServers();
  started.push(plain);
  expect(await (await fetch(`${plain.origin}/asked`, { redirect: "manual" })).text()).toBe("");
});

// The other server records a request as it arrives, before its body, so a client that stops in
// the middle of sending one is still seen to have reached it; the body joins the record once it
// ends. With a server that records a request only after its body, the record never comes and the
// answer ends first: Bun's server drops a request whose body is idle for 10 s. The limit is a hang
// guard over that wait.
test("the other server records a request whose body has not ended, and its body once it ends", async () => {
  const servers = redirectServers();
  started.push(servers);
  const sending = Promise.withResolvers<ReadableStreamDefaultController<Uint8Array>>();
  // Made for this run, so no line of the test holds a key.
  const key = `made-up-key-${crypto.randomUUID()}`;
  const answer = fetch(`${servers.elsewhere}/sent`, {
    method: "POST",
    headers: { "x-api-key": key },
    body: new ReadableStream<Uint8Array>({
      start(controller) {
        controller.enqueue(new TextEncoder().encode("the first part of a body"));
        sending.resolve(controller);
      },
    }),
  });
  const first = await Promise.race([
    servers.until(() => servers.received.length > 0).then(() => "recorded"),
    answer.then(
      () => "answered",
      () => "dropped"
    ),
  ]);
  expect(first, "the other server recorded nothing while the body was still being sent").toBe(
    "recorded"
  );
  expect(
    servers.received.map(({ method, path, headers, body }) => ({
      method,
      path,
      key: headers["x-api-key"],
      body,
    }))
  ).toEqual([{ method: "POST", path: "/sent", key, body: "" }]);
  (await sending.promise).close();
  expect((await answer).status).toBe(200);
  expect(servers.received.map(({ body }) => body)).toEqual(["the first part of a body"]);
}, 30_000);

// A client that refuses a redirect cancels the body it does not read, so a body that never ends
// holds no connection; `cancelledBodies` counts each such cancel the first server saw. The limit
// is a hang guard over the wait for the count, which a body that is never cancelled never meets.
test("an endless body that its client cancels is counted, and one it leaves is not", async () => {
  const servers = redirectServers();
  started.push(servers);
  servers.endlessBody = true;
  const kept = await fetch(`${servers.origin}/kept`, { redirect: "manual" });
  const cancelled = await fetch(`${servers.origin}/cancelled`, { redirect: "manual" });
  expect([kept.status, cancelled.status]).toEqual([307, 307]);
  await cancelled.body?.cancel();
  await servers.until(() => servers.cancelledBodies > 0);
  expect(servers.cancelledBodies).toBe(1);
  expect(kept.bodyUsed).toBe(false);
}, 30_000);
