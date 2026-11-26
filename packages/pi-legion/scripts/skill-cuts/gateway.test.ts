import { expect, spyOn, test } from "bun:test";
import { createHash, randomUUID } from "node:crypto";
import { readFileSync, writeFileSync } from "node:fs";
import path from "node:path";
import { fixtureSecret } from "@legion/test-fixtures/fixture-secret";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import { MessagesApiError, postMessages } from "../skill-scenarios/messages-api";
import type { Fetch } from "../skill-scenarios/production-dispatch";
import {
  BudgetSpent,
  bodyIdentity,
  costUsd,
  createGateway,
  deploymentOf,
  deploymentWords,
  freshIdentity,
  GATEWAY_HEADERS,
  GATEWAY_TRIES,
  type GatewayOptions,
  isResponsesBody,
  type MessagesBody,
  PRICES,
  type ResponsesBody,
  ROUTING_HEADER,
  responsesCostUsd,
  responsesTokensOf,
  type Sent as SentAt,
  toolCalls,
} from "./gateway";
import { f9Completed, fixtureReply } from "./testdata/fixtures";

// The gateway's client, on F5 and F9 (testdata/, captured as testdata/CAPTURE.md says in "F5" and
// in "F8 and F9"): the model gateway's replies to request bodies made from F2 and F8. Each F5
// reply came from `bun $TOOLS/send.ts <body.json> <F2 headers.json>`, a scratch script that posts
// the body to the gateway's `/v1/messages` with the key read inside it, and each F9 reply from
// `bun $O/send-openai.ts <body.json>`, its twin for `/v1/responses`; each body sets `stream`, so
// each reply is an SSE stream, kept with its `content-type` and request-id headers. Every fetch
// here is a fake: no test reaches a network.

const TESTDATA = path.join(import.meta.dir, "testdata");
const scratch = temporaryDirectory("skill-cuts-gateway", { real: true });

/** The body F5's replies answer: F2, a replayed history Oh My Pi 18.18.6 sent. */
const BODY = JSON.parse(
  readFileSync(path.join(TESTDATA, "F2-request.json"), "utf8")
) as MessagesBody;

/** The body F9's replies answer: F8, the Responses API body Oh My Pi 18.18.6 sent for an F2-like
 *  history with `--model openai/gpt-6.1-sol`. */
const RESPONSES_BODY = JSON.parse(
  readFileSync(path.join(TESTDATA, "F8-request.json"), "utf8")
) as ResponsesBody;

/** F5's reply `shape`, a Messages API stream. */
function f5(shape: string): Response {
  return fixtureReply(`F5-${shape}`);
}

/** F9's reply `shape`, a Responses API stream. */
function f9(shape: string): Response {
  return fixtureReply(`F9-${shape}`);
}

/** An Anthropic error answer. */
function refusal(status: number, type: string, message: string, headers = {}): Response {
  return new Response(JSON.stringify({ type: "error", error: { type, message } }), {
    status,
    headers: { "content-type": "application/json", ...headers },
  });
}

/** A key file in the scratch directory holding `key`. */
function keyFileHolding(key: string): string {
  const file = path.join(scratch, `key-${key.slice(0, 5)}-${Math.random()}`);
  writeFileSync(file, `${key}\n`, { mode: 0o600 });
  return file;
}

interface Sent {
  readonly url: string;
  readonly key: string | null;
  readonly headers: Headers;
  readonly body: unknown;
  readonly signal: AbortSignal;
}

/** A gateway whose fetch is a fake answering the n-th call (from 1) with `answer`, and the calls
 *  and back-off waits it saw. */
function world(
  answer: (n: number, sent: Sent) => Response | Promise<Response>,
  options: Partial<GatewayOptions> = {}
) {
  const sent: Sent[] = [];
  const slept: number[] = [];
  const fetch: Fetch = async (url, init) => {
    const signal = init.signal;
    if (!(signal instanceof AbortSignal)) throw new Error("the call carries no timeout signal");
    const headers = new Headers(init.headers);
    const one = {
      url,
      key: headers.get("x-api-key"),
      headers,
      body: JSON.parse(String(init.body)),
      signal,
    };
    sent.push(one);
    return answer(sent.length, one);
  };
  const gateway = createGateway({
    baseUrl: "http://gateway.test/",
    keyFile: keyFileHolding(fixtureSecret("gw")),
    concurrency: 4,
    timeoutMs: 5_000,
    spendRecorder: null,
    fetch,
    sleep: async (ms) => {
      slept.push(ms);
    },
    ...options,
  });
  return { gateway, sent, slept };
}

/** A fetch answer that comes only when the try's signal ends it: a gateway that never answers. */
function untilAborted(signal: AbortSignal): Promise<Response> {
  const { promise, reject } = Promise.withResolvers<Response>();
  signal.addEventListener("abort", () => reject(signal.reason));
  return promise;
}

/** An answer whose headers came and whose body stops after `first` until the try is ended. */
function stalledBody(signal: AbortSignal, first: string): Response {
  const body = new ReadableStream<Uint8Array>({
    start(controller) {
      controller.enqueue(new TextEncoder().encode(first));
      signal.addEventListener("abort", () => controller.error(signal.reason));
    },
  });
  return new Response(body, { status: 200, headers: { "content-type": "text/event-stream" } });
}

test("posts the body to <base>/v1/messages with the key file's key and reads a streamed reply whole", async () => {
  const key = fixtureSecret("gw");
  const { gateway, sent } = world(() => f5("skill-read"), { keyFile: keyFileHolding(key) });
  const reply = await gateway.send(BODY);
  expect(sent).toHaveLength(1);
  expect(sent[0]?.url).toBe("http://gateway.test/v1/messages");
  expect(sent[0]?.key).toBe(key);
  expect(sent[0]?.body).toEqual(BODY);
  expect(reply.requestId).toBe("req_011ChDNkcYfVK27QnBd1bAV7");
  expect(reply.message.model).toBe("claude-sonnet-5-5");
  expect(reply.message.stop_reason).toBe("tool_use");
  // The tool call's input arrives as JSON in pieces and is parsed whole when its block stops.
  expect(reply.message.content).toEqual([
    {
      type: "tool_use",
      id: "toolu_01WDxLVjvqwiht3W6X8n4wAn",
      name: "read",
      input: { path: "skill://alpha/references/x.md", i: "Reading alpha reference" },
      caller: { type: "direct" },
    },
  ]);
  // message_delta's counts are the final ones: output 84, where message_start said 16.
  expect(reply.message.usage).toMatchObject({
    input_tokens: 600,
    output_tokens: 84,
    cache_creation_input_tokens: 4836,
    cache_read_input_tokens: 8076,
    cache_creation: { ephemeral_5m_input_tokens: 0, ephemeral_1h_input_tokens: 4836 },
  });
});

/** The `anthropic-*` headers Oh My Pi 18.18.6 sent beside a body at `--thinking low` (F2), as a
 *  capture keeps them: the version, the betas its body's fields need, and one more. */
const CAPTURED_HEADERS: Record<string, string> = Object.fromEntries(
  Object.entries(
    JSON.parse(readFileSync(path.join(TESTDATA, "F2-request-thinking-low.headers.json"), "utf8"))
      .headers
  ).filter((entry): entry is [string, string] => entry[0].startsWith("anthropic-"))
);

test("sends the anthropic-* headers given with the body, on every try", async () => {
  expect(Object.keys(CAPTURED_HEADERS).sort()).toEqual([
    "anthropic-beta",
    "anthropic-dangerous-direct-browser-access",
    "anthropic-version",
  ]);
  const { gateway, sent } = world((n) =>
    n === 1 ? refusal(503, "overloaded_error", "busy") : f5("thinking")
  );
  await gateway.send(BODY, { headers: CAPTURED_HEADERS });
  expect(sent).toHaveLength(2);
  for (const one of sent) {
    for (const [name, value] of Object.entries(CAPTURED_HEADERS)) {
      expect(one.headers.get(name)).toBe(value);
    }
    expect(one.headers.get("content-type")).toBe("application/json");
    expect(one.key).not.toBeNull();
  }

  // With none given, the call carries the API version and no beta.
  const plain = world(() => f5("text"));
  await plain.gateway.send(BODY);
  expect(plain.sent[0]?.headers.get("anthropic-version")).toBe("2023-06-01");
  expect(plain.sent[0]?.headers.get("anthropic-beta")).toBeNull();
});

test("refuses a header that can carry a key, or that is not anthropic-*, by name, before any call", async () => {
  const secret = fixtureSecret("hdr");
  const { gateway, sent } = world(() => f5("text"));
  for (const name of [
    "x-api-key",
    "Authorization",
    "anthropic-api-key",
    "Cookie",
    "x-auth-token",
  ]) {
    const error = await gateway
      .send(BODY, { headers: { "anthropic-beta": "effort-2025-11-24", [name]: secret } })
      .catch((caught: unknown) => caught);
    expect(error).toBeInstanceOf(Error);
    expect(String(error)).toBe(
      `Error: the header ${name} can carry a key, so it is not sent: the gateway signs each call with the key file's key itself`
    );
  }
  const other = await gateway
    .send(BODY, { headers: { "x-app": "cli" } })
    .catch((caught: unknown) => caught);
  expect(String(other)).toBe(
    "Error: only anthropic-* headers and x-litellm-session-id are sent beside a body, not x-app"
  );
  expect(sent).toHaveLength(0);
});

/** A made-up routing id, as the runner sends one: a UUID. */
const ROUTING_ID = "5eed0000-0000-4000-8000-000000000001";

// Catches: a routing id refused beside a Messages body, or sent on the first try only, or only
// under its name in lower case: the gateway pins a call's deployment by that header alone.
test("sends x-litellm-session-id with a UUID beside a Messages body, its name in any case, on every try", async () => {
  const { gateway, sent } = world((n) =>
    n === 1 ? refusal(503, "overloaded_error", "busy") : f5("text")
  );
  await gateway.send(BODY, {
    headers: { ...CAPTURED_HEADERS, [ROUTING_HEADER]: ROUTING_ID },
  });
  const mixed = world(() => f5("text"));
  await mixed.gateway.send(BODY, { headers: { "X-LiteLLM-Session-Id": ROUTING_ID } });
  expect([...sent, ...mixed.sent].map((one) => one.headers.get("x-litellm-session-id"))).toEqual([
    ROUTING_ID,
    ROUTING_ID,
    ROUTING_ID,
  ]);
  expect(sent[1]?.headers.get("anthropic-beta")).toBe(CAPTURED_HEADERS["anthropic-beta"] ?? "");
  expect(sent[1]?.body).toEqual(BODY);
});

// Catches: a routing header sent to the Responses route, which the runner leaves as it was.
test("refuses x-litellm-session-id beside a Responses body, by name, before any call", async () => {
  const { gateway, sent } = world(() => f9("text"));
  for (const name of [ROUTING_HEADER, "X-LiteLLM-Session-Id"]) {
    const error = await gateway
      .send(RESPONSES_BODY, { headers: { [name]: ROUTING_ID } })
      .catch((caught: unknown) => caught);
    expect(String(error)).toBe(
      `Error: the header ${name} goes beside a Messages body only: a Responses API body goes with no header beside it`
    );
  }
  expect(sent).toHaveLength(0);
});

// Catches: a routing header sent with any text, which would pin calls by whatever a caller put
// there, or a refusal that prints the value.
test("refuses x-litellm-session-id whose value is not a UUID, by name, never the value, before any call", async () => {
  const { gateway, sent } = world(() => f5("text"));
  for (const value of ["", "round-1", `${ROUTING_ID}x`, ROUTING_ID.replaceAll("-", "")]) {
    const error = await gateway
      .send(BODY, { headers: { [ROUTING_HEADER]: value } })
      .catch((caught: unknown) => caught);
    expect(String(error)).toBe(
      "Error: the header x-litellm-session-id takes a UUID, and its value is none: the gateway pins a call's deployment by that id"
    );
  }
  expect(sent).toHaveLength(0);
});

// Catches: a deployment recorded under a name of its owner, or an id that does not begin with
// hex digits printed as it is; and runs counted by request rather than once each.
test("deploymentOf gives the first 8 hex digits of x-litellm-model-id, the first 8 of its SHA-256 for an id that begins otherwise, and '' for none; deploymentWords counts each run once", () => {
  const of = (id?: string) =>
    deploymentOf({ gatewayHeaders: id === undefined ? {} : { "x-litellm-model-id": id } });
  expect(of("0A1B2C3D-stand-in")).toBe("0a1b2c3d");
  expect(of("4e5f6a7b")).toBe("4e5f6a7b");
  expect(of("deployment-a")).toBe(
    createHash("sha256").update("deployment-a").digest("hex").slice(0, 8)
  );
  expect(of()).toBe("");
  expect(of("")).toBe("");
  expect(
    deploymentWords([
      ["4e5f6a7b"],
      ["0a1b2c3d", "0a1b2c3d"],
      ["0a1b2c3d"],
      [""],
      [],
      ["0a1b2c3d", "", "4e5f6a7b"],
    ])
  ).toBe("0a1b2c3d 2, 0a1b2c3d+4e5f6a7b 1, 4e5f6a7b 1, no reply 1, unnamed 1");
  expect(deploymentWords([])).toBe("no runs");
});

test("postMessages lets a header given replace the API version, never the key or the content type", async () => {
  const key = fixtureSecret("gw");
  const seen: Headers[] = [];
  const answer = await postMessages({
    url: "http://gateway.test",
    key: async () => key,
    body: "{}",
    headers: {
      "X-Api-Key": "replaced-not-a-secret",
      "Content-Type": "text/plain",
      "Anthropic-Version": "2099-01-01",
      "anthropic-beta": "effort-2025-11-24",
    },
    fetch: async (_url, init) => {
      seen.push(new Headers(init.headers));
      return new Response("answered");
    },
    sleep: async () => {},
    tries: 1,
    timeoutMs: 5_000,
    accept: (text) => text,
  });
  expect(answer).toBe("answered");
  expect(seen[0]?.get("x-api-key")).toBe(key);
  expect(seen[0]?.get("content-type")).toBe("application/json");
  expect(seen[0]?.get("anthropic-version")).toBe("2099-01-01");
  expect(seen[0]?.get("anthropic-beta")).toBe("effort-2025-11-24");
});

test("reads every block of a streamed reply: text, thinking with its signature, and tool calls", async () => {
  const { gateway } = world((n) => f5(["text", "thinking", "skill-read-and-other"][n - 1] ?? ""));
  const text = await gateway.send(BODY);
  expect(text.message.stop_reason).toBe("end_turn");
  expect(text.message.content).toEqual([
    {
      type: "text",
      text: "I haven't read `references/x.md`, which step 1 requires first, and I can't call a tool this turn, so these are the skill's three steps as written, not results from running them:\n\n1. Read `references/x.md` before the first step.\n2. Write each step's result on one line.\n3. Stop after the third step.",
    },
  ]);

  const thinking = await gateway.send(BODY);
  const [thought, said] = thinking.message.content;
  expect(thought?.type).toBe("thinking");
  expect(typeof thought?.thinking === "string" && thought.thinking.length > 0).toBe(true);
  expect(typeof thought?.signature === "string" && thought.signature.length > 0).toBe(true);
  expect(said?.type).toBe("text");
  expect(String(said?.text)).toStartWith("1. Read `references/x.md` before the first step.");

  const four = await gateway.send(BODY);
  expect(four.message.content.map((block) => [block.type, block.name, block.input])).toEqual([
    ["text", undefined, undefined],
    ["tool_use", "read", { path: "skill://beta" }],
    ["tool_use", "bash", { command: "ls" }],
    ["tool_use", "read", { path: "skill://alpha/references/x.md" }],
  ]);
});

test("retries a 429 after the wait its retry-after asks for", async () => {
  const { gateway, sent, slept } = world((n) =>
    n === 1 ? refusal(429, "rate_limit_error", "slow down", { "retry-after": "7" }) : f5("text")
  );
  const reply = await gateway.send(BODY);
  expect(reply.message.stop_reason).toBe("end_turn");
  expect(sent).toHaveLength(2);
  expect(slept).toEqual([7000]);
});

test("outlasts a rate guard that refuses for 60 s though it asks for a 1 s wait", async () => {
  // The guard looks back 60 s; it answers 429 with `retry-after: 1` until a minute has passed.
  const { gateway, sent, slept } = world(() =>
    slept.reduce((sum, ms) => sum + ms, 0) < 60_000
      ? refusal(429, "rate_limit_error", "priority guard: held back", { "retry-after": "1" })
      : f5("text")
  );
  const reply = await gateway.send(BODY);
  // The stand-in gives its one end_turn reply only once 60 s were slept.
  expect(reply.message.stop_reason).toBe("end_turn");
  expect(sent.length).toBeLessThanOrEqual(GATEWAY_TRIES);
});

test("backs off on a 5xx: 1 s, then 2 s", async () => {
  const { gateway, sent, slept } = world((n) =>
    n === 1
      ? refusal(503, "overloaded_error", "busy")
      : n === 2
        ? new Response("bad gateway", { status: 502 })
        : f5("text")
  );
  const reply = await gateway.send(BODY);
  expect(reply.message.stop_reason).toBe("end_turn");
  expect(sent).toHaveLength(3);
  expect(slept).toEqual([1000, 2000]);
});

test("does not retry a 400", async () => {
  const { gateway, sent, slept } = world(() =>
    refusal(400, "invalid_request_error", "max_tokens: too large")
  );
  const error = await gateway.send(BODY).catch((caught: unknown) => caught);
  expect(error).toBeInstanceOf(MessagesApiError);
  expect(error).toMatchObject({
    status: 400,
    message: "the model answered 400 invalid_request_error: max_tokens: too large",
  });
  expect(sent).toHaveLength(1);
  expect(slept).toEqual([]);
});

test("gives up after GATEWAY_TRIES tries of a 5xx, saying so", async () => {
  const { gateway, sent } = world(() => refusal(529, "overloaded_error", "busy"));
  await expect(gateway.send(BODY)).rejects.toThrow(
    `the model answered 529 overloaded_error: busy, on each of ${GATEWAY_TRIES} tries`
  );
  expect(sent).toHaveLength(GATEWAY_TRIES);
});

test("ends a call that passes its timeout and tries it again, its body read included", async () => {
  const { gateway, sent } = world(
    (n, { signal }) =>
      n === 1
        ? untilAborted(signal)
        : n === 2
          ? stalledBody(signal, "event: message_start\n")
          : f5("text"),
    // The timeout is messages-api.ts's own `AbortSignal.timeout`, which runs on the real clock.
    { timeoutMs: 50 }
  );
  const reply = await gateway.send(BODY);
  expect(reply.message.stop_reason).toBe("end_turn");
  expect(sent).toHaveLength(3);
  expect(sent[0]?.signal.aborted).toBe(true);
  expect(sent[1]?.signal.aborted).toBe(true);
});

test("tries again a stream cut short before message_stop", async () => {
  const whole = readFileSync(path.join(TESTDATA, "F5-text.sse"), "utf8");
  const cut = whole.slice(0, whole.indexOf("event: message_stop"));
  const { gateway, sent } = world((n) =>
    n === 1 ? new Response(cut, { status: 200 }) : f5("text")
  );
  const reply = await gateway.send(BODY);
  expect(reply.message.stop_reason).toBe("end_turn");
  expect(sent).toHaveLength(2);
});

test("sums usage per model with its cache reads and writes, and the dollars at list price", async () => {
  // F5's cache-read reply is the second send of the skill-read reply's body.
  const { gateway } = world((n) => f5(n === 1 ? "skill-read" : "cache-read"));
  const first = await gateway.send(BODY);
  const second = await gateway.send(BODY);
  // claude-sonnet-5-5 at $2 input, $10 output, $0.10 cache read and $4 a 1-hour cache write, per
  // million tokens. The first: 600 x 2 + 84 x 10 + 8076 x 0.10 + 4836 x 4 (all 1-hour) = 22191.6.
  // The second: 600 x 2 + 56 x 10 + 12912 x 0.10 = 3051.2.
  expect(first.spent).toMatchObject({
    model: "claude-sonnet-5-5",
    input: 600,
    output: 84,
    cacheRead: 8076,
    cacheWrite: 4836,
  });
  expect(first.spent.usd).toBeCloseTo(0.0221916, 10);
  expect(second.spent).toMatchObject({ input: 600, output: 56, cacheRead: 12912, cacheWrite: 0 });
  expect(second.spent.usd).toBeCloseTo(0.0030512, 10);
  const spend = gateway.spend();
  expect(Object.keys(spend.models)).toEqual(["claude-sonnet-5-5"]);
  expect(spend.models["claude-sonnet-5-5"]).toMatchObject({
    calls: 2,
    input: 1200,
    output: 140,
    cacheRead: 20988,
    cacheWrite: 4836,
  });
  expect(spend.models["claude-sonnet-5-5"]?.usd).toBeCloseTo(0.0252428, 10);
  expect(spend.usd).toBeCloseTo(0.0252428, 10);
});

test("prices a 5-minute cache write at its own rate, and a long Haiku prompt at the higher one", () => {
  const usage = {
    input_tokens: 1000,
    output_tokens: 100,
    cache_read_input_tokens: 2000,
    cache_creation_input_tokens: 3000,
    cache_creation: { ephemeral_5m_input_tokens: 1000, ephemeral_1h_input_tokens: 2000 },
  };
  // claude-opus-5-5: 1000 x 4 + 100 x 20 + 2000 x 0.20 + 1000 x 5 + 2000 x 8 = 27400.
  expect(costUsd(usage, PRICES["claude-opus-5-5"])).toBeCloseTo(0.0274, 10);
  // A write the usage does not split counts as 1-hour: 3000 x 8 in place of 1000 x 5 + 2000 x 8.
  const { cache_creation: _, ...unsplit } = usage;
  expect(costUsd(unsplit, PRICES["claude-opus-5-5"])).toBeCloseTo(0.0304, 10);
  // claude-haiku-5-5 over 100,000 prompt tokens: 100000 x 0.50 + 100 x 2.50 + 2000 x 0.05 +
  // 1000 x 0.625 + 2000 x 1 = 52975; at 100,000 or fewer, the lower rates.
  const haiku = PRICES["claude-haiku-5-5"];
  expect(costUsd({ ...usage, input_tokens: 100_000 }, haiku)).toBeCloseTo(0.052975, 10);
  expect(costUsd({ ...usage, input_tokens: 95_000 }, haiku)).toBeCloseTo(
    (95_000 * 0.1 + 100 * 0.5 + 2000 * 0.01 + 1000 * 0.125 + 2000 * 0.2) / 1e6,
    10
  );
});

test("refuses a body whose model has no price before any call, and takes a price passed in", async () => {
  const { gateway, sent } = world(() => f5("text"));
  await expect(gateway.send({ ...BODY, model: "acme-model-1" })).rejects.toThrow(
    "no list price for the model acme-model-1"
  );
  expect(sent).toHaveLength(0);

  const price = { input: 1, output: 1, cacheWriteShort: 1, cacheWriteLong: 1, cacheRead: 1 };
  const priced = world(() => f5("text"), { prices: { "acme-model-1": price } });
  // The reply says claude-sonnet-5-5 answered, so that model's own price counts it: F5-text's
  // usage at its list price is 592 x 2 + 96 x 10 + 12912 x 0.10, where the price passed in would
  // give 592 + 96 + 12912.
  const reply = await priced.gateway.send({ ...BODY, model: "acme-model-1" });
  expect(reply.spent.model).toBe("claude-sonnet-5-5");
  expect(reply.spent.usd).toBeCloseTo((592 * 2 + 96 * 10 + 12912 * 0.1) / 1e6, 10);
  expect(priced.sent).toHaveLength(1);
});

test("the budget stops new calls and says so", async () => {
  const { gateway, sent } = world(() => f5("skill-read"), { budgetUsd: 0.02 });
  const first = await gateway.send(BODY);
  expect(first.spent.usd).toBeGreaterThan(0.02);
  const error = await gateway.send(BODY).catch((caught: unknown) => caught);
  expect(error).toBeInstanceOf(BudgetSpent);
  expect(String(error)).toContain(
    "the budget of $0.02 is spent: the replies so far cost $0.0222 at list price, so no new model call starts"
  );
  expect(sent).toHaveLength(1);
  expect(gateway.spend().models["claude-sonnet-5-5"]?.calls).toBe(1);
});

test("bounds the calls in flight", async () => {
  let inFlight = 0;
  let most = 0;
  const releases: (() => void)[] = [];
  // Wakes the test when a call reaches the fake.
  let arrived: (() => void) | undefined;
  const keyFile = keyFileHolding(fixtureSecret("gw"));
  const { gateway } = world(
    async () => {
      inFlight += 1;
      most = Math.max(most, inFlight);
      const { promise, resolve } = Promise.withResolvers<void>();
      releases.push(resolve);
      arrived?.();
      await promise;
      inFlight -= 1;
      return f5("text");
    },
    { concurrency: 2, keyFile }
  );
  const all = Promise.all(Array.from({ length: 5 }, () => gateway.send(BODY)));
  for (let released = 0; released < 5; released += 1) {
    // Two calls in flight, or the one call left.
    while (releases.length < Math.min(released + 2, 5)) {
      const { promise, resolve } = Promise.withResolvers<void>();
      arrived = resolve;
      await promise;
    }
    // A call let through would have read the key and reached the fake by the end of this read.
    await Bun.file(keyFile).text();
    expect(inFlight).toBe(Math.min(2, 5 - released));
    releases[released]?.();
  }
  expect(await all).toHaveLength(5);
  expect(most).toBe(2);
});

test("makes a body's text only once its call has a place, and a call the budget refuses makes none", async () => {
  // Each body's text is made by its toJSON, which notes the body; the JSON sent is BODY's.
  const texts: string[] = [];
  const noted = (name: string): MessagesBody => ({
    ...BODY,
    toJSON: () => {
      texts.push(name);
      return BODY;
    },
  });
  const releases: (() => void)[] = [];
  let arrived: (() => void) | undefined;
  const { gateway, sent } = world(
    async () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      releases.push(resolve);
      arrived?.();
      await promise;
      return f5("skill-read");
    },
    { concurrency: 1 }
  );
  const until = async (calls: number) => {
    while (releases.length < calls) {
      const { promise, resolve } = Promise.withResolvers<void>();
      arrived = resolve;
      await promise;
    }
  };
  const both = Promise.all([gateway.send(noted("first")), gateway.send(noted("second"))]);
  await until(1);
  // The second call waits for the one place: its text is not made yet.
  expect(texts).toEqual(["first"]);
  releases[0]?.();
  await until(2);
  expect(texts).toEqual(["first", "second"]);
  releases[1]?.();
  await both;
  expect(sent.map((one) => one.body)).toEqual([BODY, BODY]);

  const spent = world(() => f5("skill-read"), { budgetUsd: 0.02 });
  await spent.gateway.send(BODY);
  const refused = await spent.gateway.send(noted("refused")).catch((caught: unknown) => caught);
  expect(refused).toBeInstanceOf(BudgetSpent);
  expect(texts).toEqual(["first", "second"]);
});

test("postMessages keeps the status of an answer whose body fails after its headers", async () => {
  const failed = await postMessages({
    url: "http://gateway.test",
    key: async () => fixtureSecret("gw"),
    body: "{}",
    fetch: async () =>
      new Response(
        new ReadableStream<Uint8Array>({
          start(controller) {
            controller.error(new Error("the made-up connection was reset"));
          },
        }),
        { status: 503 }
      ),
    sleep: async () => {},
    tries: 2,
    timeoutMs: 5_000,
    accept: (text) => text,
  }).catch((caught: unknown) => caught);
  expect(failed).toBeInstanceOf(MessagesApiError);
  expect(failed).toMatchObject({ status: 503 });
  expect(String(failed)).toContain("the made-up connection was reset, on each of 2 tries");
});

test("the key file's text is in no error, log line or thrown message", async () => {
  const key = fixtureSecret("gw");
  const keyFile = keyFileHolding(key);
  const printed = [
    spyOn(console, "log"),
    spyOn(console, "info"),
    spyOn(console, "warn"),
    spyOn(console, "error"),
    spyOn(console, "debug"),
  ];
  const errors: unknown[] = [];
  try {
    // A refusal that quotes the key it got.
    const quoting = world(() => refusal(401, "authentication_error", `bad key ${key}`), {
      keyFile,
    });
    errors.push(await quoting.gateway.send(BODY).catch((caught: unknown) => caught));
    expect(quoting.sent[0]?.key).toBe(key);
    // A connection whose failure names the key, on every try.
    const failing = world(
      () => {
        throw new Error(`socket closed after x-api-key: ${key}`);
      },
      { keyFile }
    );
    errors.push(await failing.gateway.send(BODY).catch((caught: unknown) => caught));
    // A stream that ends in an error naming a piece of the key, on every try.
    const streaming = world(
      () =>
        new Response(
          `event: error\ndata: {"type":"error","error":{"type":"api_error","message":"key ${key.slice(2, 20)}"}}\n\n`,
          { status: 200 }
        ),
      { keyFile }
    );
    errors.push(await streaming.gateway.send(BODY).catch((caught: unknown) => caught));
  } finally {
    for (const spy of printed) spy.mockRestore();
  }
  expect(errors).toHaveLength(3);
  const said = [
    ...errors.flatMap((error) =>
      error instanceof Error
        ? [error.message, String(error.stack), JSON.stringify(error), String(error)]
        : [String(error)]
    ),
    ...printed.flatMap((spy) => spy.mock.calls.map((call) => call.map(String).join(" "))),
  ];
  expect(errors.every((error) => error instanceof Error)).toBe(true);
  expect(said.join("\n")).toContain("<withheld>");
  for (let at = 0; at + 10 <= key.length; at += 1) {
    expect(said.filter((text) => text.includes(key.slice(at, at + 10)))).toEqual([]);
  }
});

test("posts a Responses body to <base>/v1/responses with the key as a bearer token, and reads its stream whole (F8, F9)", async () => {
  const key = fixtureSecret("gw");
  const { gateway, sent } = world(() => f9("skill-read"), { keyFile: keyFileHolding(key) });
  expect(isResponsesBody(RESPONSES_BODY)).toBe(true);
  expect(isResponsesBody(BODY)).toBe(false);
  const reply = await gateway.send(RESPONSES_BODY);
  expect(sent).toHaveLength(1);
  expect(sent[0]?.url).toBe("http://gateway.test/v1/responses");
  expect(sent[0]?.headers.get("authorization")).toBe(`Bearer ${key}`);
  expect(sent[0]?.key).toBeNull();
  expect(sent[0]?.headers.get("anthropic-version")).toBeNull();
  expect(sent[0]?.headers.get("content-type")).toBe("application/json");
  expect(sent[0]?.body).toEqual(RESPONSES_BODY);
  expect(reply.requestId).toBe("req_5542a27f0cee414cb1c756fb5aac9e83");
  expect(reply.message.model).toBe("gpt-6.1-sol");
  expect(reply.message.stop_reason).toBe("completed");
  // The reply's content is the response's output items as they came, and its echo of the request
  // (`instructions`, `tools`) is left out.
  expect(reply.message.content).toEqual(f9Completed("skill-read").output);
  expect(Object.keys(reply.message).sort()).toEqual([
    "content",
    "id",
    "model",
    "stop_reason",
    "usage",
  ]);
  expect(toolCalls(reply.message.content)).toEqual([
    {
      id: String(f9Completed("skill-read").output[0]?.call_id),
      name: "read",
      input: { i: "Reading alpha checklist reference", path: "skill://alpha/references/x.md" },
    },
  ]);
});

test("reads every item of a Responses reply: reasoning with its encrypted content, a message's text, and tool calls (F9)", async () => {
  const { gateway } = world((n) => f9(["text", "skill-read-and-other"][n - 1] ?? ""));
  const text = await gateway.send(RESPONSES_BODY);
  const [reasoning, message] = text.message.content;
  expect(reasoning?.type).toBe("reasoning");
  expect(String(reasoning?.encrypted_content).length).toBeGreaterThan(100);
  expect(message).toMatchObject({
    type: "message",
    role: "assistant",
    content: [
      {
        type: "output_text",
        text: "1. Read `references/x.md`.\n2. Write each step’s result on one line.\n3. Stop after the third step.",
      },
    ],
  });
  expect(toolCalls(text.message.content)).toEqual([]);

  const both = await gateway.send(RESPONSES_BODY);
  expect(toolCalls(both.message.content).map((call) => [call.name, call.input])).toEqual([
    ["read", { i: "Reading requested beta skill", path: "skill://beta" }],
    [
      "bash",
      {
        i: "Listing current directory entries",
        command: "ls",
        timeout: null,
        cwd: null,
        pty: null,
        async: null,
        name: null,
        ready: null,
      },
    ],
  ]);
});

test("sums a Responses reply's usage, its cache reads and writes and its reasoning, at gpt-6.1-sol's list price, which is the gateway's own cost (F9)", async () => {
  // F9's cache-read reply is the second send of the skill-read reply's body.
  const { gateway } = world((n) => f9(["skill-read", "cache-read", "text"][n - 1] ?? ""));
  const first = await gateway.send(RESPONSES_BODY);
  const second = await gateway.send(RESPONSES_BODY);
  const third = await gateway.send(RESPONSES_BODY);
  // gpt-6.1-sol at $2 input, $10 output, $0.10 a cache read and $2.50 a 30-minute cache write, per
  // million tokens; its input tokens count the reads and writes among them. The first:
  // (7985 - 7982) x 2 + 30 x 10 + 7982 x 2.50 = 20261.
  expect(first.spent).toMatchObject({
    model: "gpt-6.1-sol",
    input: 3,
    output: 30,
    cacheRead: 0,
    cacheWrite: 7982,
    reasoning: 0,
  });
  expect(first.spent.usd).toBeCloseTo(0.020261, 10);
  // The second send of that body reads what the first wrote: 3 x 2 + 30 x 10 + 7982 x 0.10.
  expect(second.spent).toMatchObject({ input: 3, output: 30, cacheRead: 7982, cacheWrite: 0 });
  expect(second.spent.usd).toBeCloseTo(0.0011042, 10);
  // The text reply: 3 x 2 + 76 x 10 + 8083 x 0.10 + 19 x 2.50, 43 of its 76 output tokens
  // reasoning.
  expect(third.spent).toMatchObject({
    input: 3,
    output: 76,
    cacheRead: 8083,
    cacheWrite: 19,
    reasoning: 43,
  });
  expect(third.spent.usd).toBeCloseTo(0.0016218, 10);
  // Each is the gateway's own count of the reply, its usage's `cost`.
  for (const [reply, shape] of [
    [first, "skill-read"],
    [second, "cache-read"],
    [third, "text"],
  ] as const) {
    expect(reply.spent.usd).toBeCloseTo(f9Completed(shape).usage.cost, 10);
  }
  const spend = gateway.spend();
  expect(spend.models["gpt-6.1-sol"]).toMatchObject({
    calls: 3,
    input: 9,
    output: 136,
    cacheRead: 16065,
    cacheWrite: 8001,
    reasoning: 43,
  });
  expect(spend.usd).toBeCloseTo(0.020261 + 0.0011042 + 0.0016218, 10);
});

test("prices gpt-6.1-sol's long context, over 272,000 input tokens, at its higher rates, and a write the usage does not split as a 12-hour one", () => {
  const usage = {
    input_tokens: 300_000,
    output_tokens: 100,
    input_tokens_details: {
      cached_tokens: 200_000,
      cache_write_tokens: 50_000,
      cache_write_tokens_details: { ttl_30m_tokens: 20_000, ttl_12h_tokens: 30_000 },
    },
    output_tokens_details: { reasoning_tokens: 40 },
  };
  const price = PRICES["gpt-6.1-sol"];
  if (price === undefined) throw new Error("PRICES has no gpt-6.1-sol");
  expect(responsesTokensOf(usage)).toEqual({
    input: 50_000,
    output: 100,
    cacheRead: 200_000,
    cacheWrite: 50_000,
    reasoning: 40,
  });
  // 50000 x 4 + 100 x 15 + 200000 x 0.20 + 20000 x 5 + 30000 x 10 = 641500.
  expect(responsesCostUsd(usage, price)).toBeCloseTo(0.6415, 10);
  // At 272,000 input tokens, the lower rates: 22000 x 2 + 100 x 10 + 200000 x 0.10 + 20000 x 2.50 +
  // 30000 x 5 = 265000.
  const short = { ...usage, input_tokens: 272_000 };
  expect(responsesCostUsd(short, price)).toBeCloseTo(0.265, 10);
  // Unsplit, the 50000 written are priced at the 12-hour rate:
  // 22000 x 2 + 1000 + 20000 + 50000 x 5.
  const unsplit = {
    ...short,
    input_tokens_details: { cached_tokens: 200_000, cache_write_tokens: 50_000 },
  };
  expect(responsesCostUsd(unsplit, price)).toBeCloseTo(0.315, 10);
});

test("a Responses body goes with no header beside it: one given is refused by name, before any call", async () => {
  const { gateway, sent } = world(() => f9("text"));
  const error = await gateway
    .send(RESPONSES_BODY, { headers: { "anthropic-version": "2023-06-01" } })
    .catch((caught: unknown) => caught);
  expect(String(error)).toBe(
    "Error: a Responses API body goes with no header beside it, not anthropic-version: the anthropic-* headers are the Messages API's"
  );
  expect(sent).toHaveLength(0);
});

test("tries again a Responses stream cut short before response.completed, or ended by an error event", async () => {
  const whole = readFileSync(path.join(TESTDATA, "F9-text.sse"), "utf8");
  const cut = whole.slice(0, whole.indexOf('data: {"type":"response.completed"'));
  const failed = 'data: {"type":"error","code":"server_error","message":"made up"}\n\n';
  const { gateway, sent, slept } = world((n) =>
    n === 1 ? new Response(cut) : n === 2 ? new Response(failed) : f9("text")
  );
  const reply = await gateway.send(RESPONSES_BODY);
  expect(reply.message.stop_reason).toBe("completed");
  expect(sent).toHaveLength(3);
  expect(slept).toEqual([1000, 2000]);

  const failing = world(() => new Response(failed));
  await expect(failing.gateway.send(RESPONSES_BODY)).rejects.toThrow(
    `the stream ended in an error: server_error: made up, on each of ${GATEWAY_TRIES} tries`
  );
  const short = world(() => new Response(cut));
  await expect(short.gateway.send(RESPONSES_BODY)).rejects.toThrow(
    "the stream ended before response.completed"
  );
});

// Catches: a provider's content-policy refusal tried again GATEWAY_TRIES times, each try paid
// for and refused the same, with 91 s of back-off between them; and a refusal thrown as no
// MessagesApiError, so the request log named no error of its own and no status.
test("a stream whose error's type (a Responses error's code) ends in _policy, the provider's content policy, throws at once as a MessagesApiError of status 200 and is not tried again, on either route, and counts no spend", async () => {
  const said = "This content was flagged for possible cybersecurity risk.";
  const refused = {
    name: "MessagesApiError",
    message: `the stream ended in an error: cyber_policy: ${said}`,
    status: 200,
  };
  const messages = world(
    () =>
      new Response(
        `event: error\ndata: ${JSON.stringify({ type: "error", error: { type: "cyber_policy", message: said } })}\n\n`
      )
  );
  const fromMessages = await messages.gateway
    .send({ ...BODY, stream: true })
    .catch((caught: unknown) => caught);
  expect(fromMessages).toBeInstanceOf(MessagesApiError);
  expect(fromMessages).toMatchObject(refused);
  expect({ sent: messages.sent.length, slept: messages.slept }).toEqual({ sent: 1, slept: [] });
  expect(messages.gateway.spend().usd).toBe(0);

  const responses = world(
    () =>
      new Response(
        `data: ${JSON.stringify({ type: "error", code: "cyber_policy", message: said })}\n\n`
      )
  );
  const fromResponses = await responses.gateway
    .send(RESPONSES_BODY)
    .catch((caught: unknown) => caught);
  expect(fromResponses).toBeInstanceOf(MessagesApiError);
  expect(fromResponses).toMatchObject(refused);
  expect({ sent: responses.sent.length, slept: responses.slept }).toEqual({ sent: 1, slept: [] });
  expect(responses.gateway.spend().usd).toBe(0);
});

test("postMessages on the bearer route posts to its path with the key as authorization and no API version, and no header given replaces the key", async () => {
  const key = fixtureSecret("gw");
  const seen: { url: string; headers: Headers }[] = [];
  await postMessages({
    url: "http://gateway.test",
    path: "/v1/responses",
    auth: "bearer",
    key: async () => key,
    body: "{}",
    headers: { Authorization: "Bearer replaced-not-a-secret" },
    fetch: async (url, init) => {
      seen.push({ url, headers: new Headers(init.headers) });
      return new Response("answered");
    },
    sleep: async () => {},
    tries: 1,
    timeoutMs: 5_000,
    accept: (text) => text,
  });
  expect(seen[0]?.url).toBe("http://gateway.test/v1/responses");
  expect(seen[0]?.headers.get("authorization")).toBe(`Bearer ${key}`);
  expect(seen[0]?.headers.get("x-api-key")).toBeNull();
  expect(seen[0]?.headers.get("anthropic-version")).toBeNull();
});

const UUID = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

// Catches: a fresh id of another shape than the host's, a body changed beyond its id, or one id
// given twice.
test("freshIdentity draws the session id again in its own shape and changes nothing else of the body", () => {
  // Oh My Pi's Messages body (F2): `{"session_id": <uuid>}`.
  const omp = freshIdentity(BODY);
  const ompId = JSON.parse(omp.id ?? "{}");
  expect(Object.keys(ompId)).toEqual(["session_id"]);
  expect(ompId.session_id).toMatch(UUID);
  expect(omp.id).not.toBe(bodyIdentity(BODY));
  expect(omp.body).toEqual({ ...BODY, metadata: { ...Object(BODY.metadata), user_id: omp.id } });
  expect(bodyIdentity(omp.body)).toBe(omp.id);
  // Claude Code's (F3): its device id stays 64 hex digits, its empty account id stays empty.
  const claude = JSON.parse(
    readFileSync(path.join(TESTDATA, "F3-request-first.json"), "utf8")
  ) as MessagesBody;
  const recorded = JSON.parse(bodyIdentity(claude) ?? "{}");
  const drawn = JSON.parse(freshIdentity(claude).id ?? "{}");
  expect(Object.keys(drawn)).toEqual(Object.keys(recorded));
  expect(drawn.device_id).toMatch(/^[0-9a-f]{64}$/);
  expect(drawn.device_id).not.toBe(recorded.device_id);
  expect(drawn.account_uuid).toBe("");
  expect(drawn.session_id).toMatch(UUID);
  expect(drawn.session_id).not.toBe(recorded.session_id);
  // A Responses body (F8): its `prompt_cache_key`, a UUID.
  const responses = freshIdentity(RESPONSES_BODY);
  expect(responses.id).toMatch(UUID);
  expect(responses.id).not.toBe(RESPONSES_BODY.prompt_cache_key);
  expect(responses.body).toEqual({ ...RESPONSES_BODY, prompt_cache_key: responses.id });
  // A body with no id is given back as it is, or given Oh My Pi's shape of one.
  const { metadata: _metadata, ...bare } = BODY;
  expect(freshIdentity(bare as MessagesBody)).toEqual({ body: bare, id: null });
  expect(bodyIdentity(bare as MessagesBody)).toBeNull();
  const added = freshIdentity(bare as MessagesBody, true);
  expect(JSON.parse(added.id ?? "{}").session_id).toMatch(UUID);
  expect(added.body).toEqual({ ...bare, metadata: { user_id: added.id } });
  // No two draws give one id.
  const ids = Array.from({ length: 200 }, () => freshIdentity(BODY).id);
  expect(new Set(ids).size).toBe(200);
  // The body given is never changed.
  expect(BODY.metadata).toEqual(
    JSON.parse(readFileSync(path.join(TESTDATA, "F2-request.json"), "utf8")).metadata
  );
});

// Catches: an id whose words hold the letters a to f, redrawn letter by letter, so its words change
// and the id leaves its host's shape.
test("freshIdentity keeps the words of an id and draws again only its runs of hex digits", () => {
  const digest = createHash("sha256").update("a made-up device").digest("hex");
  const recorded = `user_${digest}_account__session_${randomUUID()}`;
  const body = { ...BODY, metadata: { user_id: recorded } } as MessagesBody;
  const drawn = freshIdentity(body).id ?? "";
  const shape =
    /^user_([0-9a-f]{64})_account__session_([0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12})$/;
  const [, device, session] = shape.exec(drawn) ?? [];
  const [, recordedDevice, recordedSession] = shape.exec(recorded) ?? [];
  expect(device).toBeString();
  expect(device).not.toBe(recordedDevice);
  expect(session).not.toBe(recordedSession);
});

// Catches: a request log that cannot tell which deployment answered, or when a call was sent and
// with how many others.
test("a reply carries the gateway's headers, the provider's request id, its send time and the calls in flight then", async () => {
  const headers = {
    "content-type": "text/event-stream",
    "x-litellm-model-id": "deployment-a",
    "x-litellm-model-group": "claude-sonnet-5-5",
    "x-litellm-attempted-fallbacks": "0",
    "x-litellm-attempted-retries": "1",
    "llm_provider-request-id": "req_provider_made_up",
    "x-litellm-key-spend": "not recorded",
  };
  const text = readFileSync(path.join(TESTDATA, "F5-text.sse"), "utf8");
  const releases: (() => void)[] = [];
  const arrived: (() => void)[] = [];
  const { gateway } = world(
    async () => {
      const { promise, resolve } = Promise.withResolvers<void>();
      releases.push(resolve);
      arrived.shift()?.();
      await promise;
      return new Response(text, { status: 200, headers });
    },
    { concurrency: 2 }
  );
  const told: SentAt[] = [];
  const before = new Date().toISOString();
  const replies = Array.from({ length: 3 }, () =>
    gateway.send(BODY, { onSent: (sent) => told.push(sent) })
  );
  // Two calls go at once; the third gets its place when the first ends.
  while (releases.length < 2) {
    const { promise, resolve } = Promise.withResolvers<void>();
    arrived.push(resolve);
    await promise;
  }
  expect(told.map((sent) => sent.inFlight)).toEqual([1, 2]);
  releases[0]?.();
  while (releases.length < 3) {
    const { promise, resolve } = Promise.withResolvers<void>();
    arrived.push(resolve);
    await promise;
  }
  releases[1]?.();
  releases[2]?.();
  const done = await Promise.all(replies);
  expect(told.map((sent) => sent.inFlight)).toEqual([1, 2, 2]);
  for (const [index, reply] of done.entries()) {
    expect(reply.gatewayHeaders).toEqual({
      "x-litellm-model-id": "deployment-a",
      "x-litellm-model-group": "claude-sonnet-5-5",
      "x-litellm-attempted-fallbacks": "0",
      "x-litellm-attempted-retries": "1",
      "llm_provider-request-id": "req_provider_made_up",
    });
    expect(reply.providerRequestId).toBe("req_provider_made_up");
    expect(reply.sentAt).toBe(told[index]?.at ?? "");
    expect(reply.sentAt >= before).toBe(true);
    expect(reply.inFlight).toBe(told[index]?.inFlight ?? 0);
  }
  expect(GATEWAY_HEADERS).toContain("request-id");
  // F5's own reply carries the provider's id as `llm_provider-request-id` and `request-id`.
  const fixture = await world(() => f5("text")).gateway.send(BODY);
  expect(fixture.providerRequestId).toBe(fixture.requestId ?? "");
  expect(fixture.gatewayHeaders).toEqual({
    "llm_provider-request-id": fixture.requestId ?? "",
    "request-id": fixture.requestId ?? "",
  });
  // A call that fails was still told when it was sent.
  const failing: SentAt[] = [];
  const { gateway: refusing } = world(() => refusal(400, "invalid_request_error", "made up"));
  await expect(refusing.send(BODY, { onSent: (sent) => failing.push(sent) })).rejects.toThrow();
  expect(failing.map((sent) => sent.inFlight)).toEqual([1]);
});
