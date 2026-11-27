import { afterAll, expect, test } from "bun:test";
import { readFileSync } from "node:fs";
import path from "node:path";
import { EnvelopeSchema } from "@legion/contracts";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";

// envoy-standin.ts is the front of the batch's Envoy listener that a case run's pane reaches. The
// listener here answers a send as cmd/listener/api.go's sendHandler answers each of the four
// outcomes this front tells apart, in the words that handler writes.

const work = temporaryDirectory("envoy-standin");
const LIVE_ENVELOPE = { event_id: "evt-live", note: "the listener's own answer" };

/** What the listener was sent, beside the front's own. */
const seen: { method: string; path: string; authorization: string | null; body: string }[] = [];
const listener = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    const body = await request.text();
    seen.push({
      method: request.method,
      path: url.pathname,
      authorization: request.headers.get("authorization"),
      body,
    });
    if (url.pathname === "/v1/messages/publish")
      return Response.json({ error: "no holder for this role" }, { status: 404 });
    if (url.pathname !== "/v1/messages/send") return Response.json([{ id: "ses-live" }]);
    const sent = JSON.parse(body);
    switch (sent.target_session) {
      case "ses-live":
        return Response.json(LIVE_ENVELOPE);
      case "ses-forbidden":
        return Response.json({ error: "the token holds no such session" }, { status: 403 });
      case "ses-other-404":
        return Response.json({ error: "this route has no such page" }, { status: 404 });
      default:
        // sendHandler's noLiveSession and unknownSession.
        return Response.json(
          { error: `no live session ${sent.target_session}; this listener does not know it` },
          { status: 404 }
        );
    }
  },
});

const log = path.join(work, "calls.jsonl");
const front = Bun.spawn(
  [
    process.execPath,
    path.join(import.meta.dir, "envoy-standin.ts"),
    `http://127.0.0.1:${listener.port}`,
    log,
  ],
  { stdout: "pipe", stderr: "inherit" }
);
afterAll(() => {
  front.kill();
  listener.stop(true);
});
const reader = front.stdout.getReader();
const first = await reader.read();
const base = /listening on (http:\/\/127\.0\.0\.1:\d+)/.exec(
  new TextDecoder().decode(first.value)
)?.[1];
if (base === undefined) throw new Error("the front did not say where it listens");

const send = (target: string, extra: Record<string, unknown> = {}) =>
  fetch(`${base}/v1/messages/send`, {
    method: "POST",
    headers: { authorization: "Bearer listener-token", "content-type": "application/json" },
    body: JSON.stringify({
      source: "agent",
      source_session: "ses-role",
      target_session: target,
      message: "First line.\nSecond line.",
      idempotency_key: "key-1",
      ...extra,
    }),
  });
const rows = () =>
  readFileSync(log, "utf8")
    .split("\n")
    .filter((line) => line !== "")
    .map((line) => JSON.parse(line));

test("a send to a session the listener has never heard of is answered as a send to a live session is", async () => {
  const answered = await send("ses-world", { expects_reply: false, urgency: "high" });
  expect(answered.status).toBe(200);
  const envelope = await answered.json();
  // The envelope is the listener's own shape (contracts' EnvelopeSchema, which the client parses).
  expect(() => EnvelopeSchema.parse(envelope)).not.toThrow();
  expect(envelope).toMatchObject({
    source: "agent",
    source_session: "ses-role",
    topic: "notifications.agent.ses-world",
    dedupe_key: "agent.ses-world.key-1",
    payload_summary: "First line.",
    payload: "First line.\nSecond line.",
    urgency: "high",
    expects_reply: false,
    recipient: "ses-world",
    recipient_state: "live",
    state: "connected",
  });
  // A message that is its own summary carries no separate payload, as the listener stores it.
  const short = (await (await send("ses-world", { message: "One line." })).json()) as Record<
    string,
    unknown
  >;
  expect(short.payload_summary).toBe("One line.");
  expect(short).not.toHaveProperty("payload");
});

test("every other answer is the listener's own, and the request reached it as it was sent", async () => {
  const before = seen.length;
  const live = await send("ses-live");
  expect([live.status, await live.json()]).toEqual([200, LIVE_ENVELOPE]);
  const forbidden = await send("ses-forbidden");
  expect([forbidden.status, await forbidden.json()]).toEqual([
    403,
    { error: "the token holds no such session" },
  ]);
  const elsewhere = await send("ses-other-404");
  expect([elsewhere.status, await elsewhere.json()]).toEqual([
    404,
    { error: "this route has no such page" },
  ]);
  const publish = await fetch(`${base}/v1/messages/publish`, {
    method: "POST",
    body: JSON.stringify({ topic: "notifications.role.acme.nobody", message: "m" }),
  });
  expect([publish.status, await publish.json()]).toEqual([
    404,
    { error: "no holder for this role" },
  ]);
  expect(((await (await fetch(`${base}/v1/sessions`)).json()) as unknown[])[0]).toEqual({
    id: "ses-live",
  });
  const reached = seen.slice(before);
  expect(reached[0]).toMatchObject({
    method: "POST",
    path: "/v1/messages/send",
    authorization: "Bearer listener-token",
  });
  expect(JSON.parse(reached[0]?.body ?? "{}")).toMatchObject({
    target_session: "ses-live",
    idempotency_key: "key-1",
  });
});

test("each send is a line of calls.jsonl with the status the pane was answered and its exit, for the stand-ins that read the file", async () => {
  const lines = rows();
  const byTarget = Object.fromEntries(lines.map((line) => [line.target, line]));
  expect(byTarget["ses-world"]).toMatchObject({
    as: "envoy",
    route: "POST /v1/messages/send",
    status: 200,
    exit: 0,
    by: "stand-in",
  });
  expect(byTarget["ses-live"]).toMatchObject({ status: 200, exit: 0, by: "listener" });
  expect(byTarget["ses-forbidden"]).toMatchObject({ status: 403, exit: 1, by: "listener" });
  expect(byTarget["ses-other-404"]).toMatchObject({ status: 404, exit: 1, by: "listener" });
  for (const line of lines) expect(Number.isFinite(Date.parse(line.at))).toBe(true);
});
