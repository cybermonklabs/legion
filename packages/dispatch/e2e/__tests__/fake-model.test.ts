import { afterAll, beforeAll, expect, test } from "bun:test";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { fileURLToPath } from "node:url";

import type { FakeModelRequests, FakeModelRule } from "../fake-model";
import { withoutHarnessPort } from "../harness-ports";

// The stand-in model is the browser harness's own fixture (e2e/fake-model.ts), so these tests run
// here, against a stand-in of their own on a port the OS chooses, read from its listening line, as
// e2e/__tests__/fake-envoy.test.ts runs the fake Envoy.
const fakeModelModule = fileURLToPath(new URL("../fake-model.ts", import.meta.url));

let origin = "";
let stop = async () => {};

beforeAll(async () => {
  const fake = spawn("bun", [fakeModelModule], {
    env: withoutHarnessPort("fakeModel"),
    stdio: ["ignore", "pipe", "inherit"],
  });
  const exited = once(fake, "exit");
  stop = async () => {
    fake.kill();
    await exited;
  };
  const ready = Promise.withResolvers<number>();
  let output = "";
  fake.stdout.setEncoding("utf8");
  fake.stdout.on("data", (chunk: string) => {
    output += chunk;
    const listening = /fake model listener on 127\.0\.0\.1:(\d+)\n/.exec(output);
    if (listening !== null) ready.resolve(Number(listening[1]));
  });
  fake.once("exit", (code) => {
    ready.reject(new Error(`the stand-in model exited ${code} before it listened: ${output}`));
  });
  fake.once("error", ready.reject);
  origin = `http://127.0.0.1:${await ready.promise}`;
});

afterAll(async () => {
  await stop();
});

function put(path: string, body: unknown): Promise<Response> {
  return Bun.fetch(`${origin}${path}`, {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json" },
    method: "PUT",
  });
}

function post(path: string, body: unknown): Promise<Response> {
  return Bun.fetch(`${origin}${path}`, {
    body: JSON.stringify(body),
    headers: { "Content-Type": "application/json" },
    method: "POST",
  });
}

test("each protocol is answered in its own shape, whole or streamed, by the first rule that matches", async () => {
  expect((await put("/__fixture/reset", {})).ok).toBe(true);
  const rules: FakeModelRule[] = [
    { contains: "This names the problem.", model: "reviewer", reply: { text: '{"findings":[]}' } },
    { model: "records", once: true, reply: { text: "first" } },
    { model: "records", once: true, reply: { text: "second" } },
  ];
  expect((await put("/__fixture/rules", rules)).ok).toBe(true);

  const anthropic = {
    max_tokens: 100,
    messages: [{ content: [{ text: "This names the problem.", type: "text" }], role: "user" }],
    model: "reviewer",
    system: [{ text: "You review.", type: "text" }],
  };
  const whole = await post("/v1/messages", anthropic);
  expect(whole.status).toBe(200);
  expect(await whole.json()).toMatchObject({
    content: [{ text: '{"findings":[]}', type: "text" }],
    model: "reviewer",
    stop_reason: "end_turn",
    type: "message",
  });
  const streamed = await post("/v1/messages", { ...anthropic, stream: true });
  expect(streamed.headers.get("content-type")).toBe("text/event-stream");
  const events = await streamed.text();
  expect(events).toContain("event: content_block_delta");
  expect(events).toContain('"text":"{\\"findings\\":[]}"');
  expect(events).toContain("event: message_stop");

  const chat = {
    messages: [{ content: "Is this a question about words?", role: "user" }],
    model: "records",
  };
  const first = await post("/models/chat/completions?api-version=2024-05-01-preview", chat);
  expect(await first.json()).toMatchObject({
    choices: [{ finish_reason: "stop", message: { content: "first", role: "assistant" } }],
    object: "chat.completion",
  });
  const second = await post("/chat/completions", {
    ...chat,
    stream: true,
    stream_options: { include_usage: true },
  });
  const chunks = await second.text();
  expect(chunks).toContain('"content":"second"');
  expect(chunks).toContain('"total_tokens":');
  expect(chunks.endsWith("data: [DONE]\n\n")).toBe(true);

  // Both once rules are used up, and no rule answers what nobody scripted.
  const unscripted = await post("/chat/completions", chat);
  expect(unscripted.status).toBe(500);
  expect(await unscripted.json()).toMatchObject({
    error: { message: "the stand-in model has no reply scripted for this request" },
  });

  const { requests } = (await (
    await Bun.fetch(`${origin}/__fixture/requests`)
  ).json()) as FakeModelRequests;
  expect(requests.map((request) => [request.protocol, request.model, request.stream])).toEqual([
    ["anthropic", "reviewer", false],
    ["anthropic", "reviewer", true],
    ["chat", "records", false],
    ["chat", "records", true],
    ["chat", "records", false],
  ]);
  expect(requests[0]?.text).toBe("You review.\nThis names the problem.");
  expect(requests[2]).toMatchObject({
    path: "/models/chat/completions",
    query: "api-version=2024-05-01-preview",
  });

  expect((await put("/__fixture/requests", {})).ok).toBe(true);
  const forgotten = (await (
    await Bun.fetch(`${origin}/__fixture/requests`)
  ).json()) as FakeModelRequests;
  expect(forgotten.requests).toEqual([]);
});

test("a scripted error is answered in each protocol's error shape, and a malformed script is refused", async () => {
  expect((await put("/__fixture/reset", {})).ok).toBe(true);
  expect((await put("/__fixture/rules", [{ reply: { status: 429, text: "slow down" } }])).ok).toBe(
    true
  );
  const anthropic = await post("/v1/messages", { messages: [], model: "m" });
  expect(anthropic.status).toBe(429);
  expect(await anthropic.json()).toMatchObject({ error: { message: "slow down" }, type: "error" });
  const chat = await post("/chat/completions", { messages: [], model: "m" });
  expect(chat.status).toBe(429);
  expect(await chat.json()).toMatchObject({ error: { message: "slow down" } });

  expect((await put("/__fixture/rules", [{ model: 7 }])).status).toBe(400);
});
