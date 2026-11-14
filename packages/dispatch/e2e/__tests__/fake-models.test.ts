import { afterAll, beforeAll, describe, expect, test } from "bun:test";
import { spawn } from "node:child_process";
import { once } from "node:events";
import { mkdtempSync, readFileSync, rmSync } from "node:fs";
import { tmpdir } from "node:os";
import { join } from "node:path";
import { fileURLToPath } from "node:url";

import type { ChatScript, FakeChatCalls } from "../fake-chat-model";
import type { DecisionScript, FakeDecisionCalls } from "../fake-decision-model";
import { type ModelsFiles, makeModelsMaterial, modelsDirVariable } from "../harness-models";
import { withoutHarnessPort } from "../harness-ports";

// The fake models are the models lane's own fixtures (e2e/fake-chat-model.ts,
// e2e/fake-decision-model.ts), so what they answer is held here, where a spec author reads what
// a script does without a browser: each test file starts a fake of its own, over https with a
// models directory of its own as the harness makes one (e2e/harness-models.ts), on a port the OS
// chooses, and speaks to it as the server does, trusting that directory's certificate alone.

let dir: string;
let files: ModelsFiles;

beforeAll(() => {
  dir = mkdtempSync(join(tmpdir(), "dispatch-e2e-models-test-"));
  files = makeModelsMaterial(dir);
});

afterAll(() => {
  rmSync(dir, { force: true, recursive: true });
});

interface Fake {
  readonly origin: string;
  call(path: string, init?: RequestInit & { bearer?: string | null }): Promise<Response>;
  stop(): Promise<void>;
}

/** Starts `module` as the harness does, with its port unset so the OS chooses one. */
async function startFake(
  module: string,
  key: "fakeChatModel" | "fakeDecisionModel",
  report: RegExp,
  token: string
): Promise<Fake> {
  const env = { ...withoutHarnessPort(key), DISPATCH_E2E_MODELS: "fake", [modelsDirVariable]: dir };
  const path = fileURLToPath(new URL(`../${module}`, import.meta.url));
  const fake = spawn("bun", [path], { env, stdio: ["ignore", "pipe", "inherit"] });
  const exited = once(fake, "exit");
  const stop = async () => {
    fake.kill();
    await exited;
  };
  const ready = Promise.withResolvers<number>();
  let output = "";
  fake.stdout.setEncoding("utf8");
  fake.stdout.on("data", (chunk: string) => {
    output += chunk;
    const listening = report.exec(output);
    if (listening !== null) ready.resolve(Number(listening[1]));
  });
  fake.once("exit", (code) => ready.reject(new Error(`${module} exited ${code}: ${output}`)));
  const port = await ready.promise.catch(async (error) => {
    await stop();
    throw error;
  });
  const origin = `https://127.0.0.1:${port}`;
  const bearer = `Bearer ${readFileSync(token, "utf8").trim()}`;
  const ca = readFileSync(files.certificate, "utf8");
  return {
    call: (route, init = {}) => {
      const { bearer: given = bearer, ...rest } = init;
      const headers = new Headers(rest.headers);
      if (given !== null) headers.set("Authorization", given);
      return Bun.fetch(`${origin}${route}`, { ...rest, headers, tls: { ca } });
    },
    origin,
    stop,
  };
}

/** The content and the tool calls a streamed completion carried, joined as a client joins them,
 *  and the finish reason it ended on. */
async function streamed(response: Response) {
  expect(response.status).toBe(200);
  expect(response.headers.get("content-type")).toContain("text/event-stream");
  const events = (await response.text()).split("\n\n").filter((event) => event !== "");
  expect(events.at(-1)).toBe("data: [DONE]");
  let content = "";
  let finish: string | null = null;
  const calls: { id?: string; name?: string; arguments: string }[] = [];
  for (const event of events.slice(0, -1)) {
    const frame = JSON.parse(event.replace(/^data: /, ""));
    const [choice] = frame.choices;
    content += choice.delta.content ?? "";
    for (const delta of choice.delta.tool_calls ?? []) {
      calls[delta.index] ??= { arguments: "" };
      const call = calls[delta.index];
      if (delta.id !== undefined) call.id = delta.id;
      if (delta.function.name !== undefined) call.name = delta.function.name;
      call.arguments += delta.function.arguments ?? "";
    }
    finish = choice.finish_reason ?? finish;
  }
  return { calls, content, frames: events.length - 1, finish };
}

const structuredFormat = { json_schema: { name: "final" }, type: "json_schema" };

function completion(body: Record<string, unknown>, bearer?: string | null) {
  return {
    bearer,
    body: JSON.stringify({ messages: [{ content: "q", role: "user" }], model: "m", ...body }),
    headers: { "Content-Type": "application/json" },
    method: "POST",
  };
}

describe("the fake chat model", () => {
  let chat: Fake;

  beforeAll(async () => {
    chat = await startFake(
      "fake-chat-model.ts",
      "fakeChatModel",
      /fake chat model listener on 127\.0\.0\.1:(\d+)\n/,
      files.chatToken
    );
  });

  afterAll(async () => {
    await chat?.stop();
  });

  const script = async (value: ChatScript) => {
    expect((await chat.call("/__fixture/reset", { method: "PUT" })).status).toBe(200);
    const put = await chat.call("/__fixture/script", {
      body: JSON.stringify(value),
      method: "PUT",
    });
    expect(put.status).toBe(200);
  };
  const calls = async () => (await (await chat.call("/__fixture/calls")).json()) as FakeChatCalls;

  test("a Final streams as the schema's object to a structured call, answer first, and as its answer to a plain one", async () => {
    const final = {
      answer: "DEMO-12 is open.",
      arguments: { dispatch_claim: [{ key: "DEMO-12" }] },
      block: { kind: "metric", spec: { value: 3 }, title: "Open" },
    };
    await script({ default: { final } });
    const structured = await streamed(
      await chat.call("/chat/completions", completion({ response_format: structuredFormat }))
    );
    expect(structured.frames).toBeGreaterThan(3);
    expect(structured.content.startsWith('{"answer":')).toBe(true);
    expect(JSON.parse(structured.content)).toEqual({
      answer: final.answer,
      arguments: JSON.stringify(final.arguments),
      block: { kind: "metric", spec: JSON.stringify(final.block.spec), title: "Open" },
    });
    expect(structured.finish).toBe("stop");
    const plain = await streamed(await chat.call("/chat/completions", completion({})));
    expect(plain.content).toBe(final.answer);
    expect(await calls()).toMatchObject({ abandoned: 0, calls: 2, structured: 1 });
  });

  test("a call without the token's bearer is refused and not answered", async () => {
    await script({ default: { text: "never" } });
    const refused = await chat.call("/chat/completions", completion({}, "Bearer wrong"));
    expect(refused.status).toBe(401);
    expect(await calls()).toMatchObject({ calls: 0, unauthorized: 1 });
  });

  test("a refused structured call leaves its reply for the prose call that follows it", async () => {
    await script({
      replies: [{ refuseStructured: true, text: "first" }, { text: "second" }],
    });
    const refused = await chat.call(
      "/chat/completions",
      completion({ response_format: structuredFormat })
    );
    expect(refused.status).toBe(400);
    expect((await streamed(await chat.call("/chat/completions", completion({})))).content).toBe(
      "first"
    );
    expect((await streamed(await chat.call("/chat/completions", completion({})))).content).toBe(
      "second"
    );
    const none = await chat.call("/chat/completions", completion({}));
    expect(none.status).toBe(500);
  });

  test("tool calls come first and the answer goes to the call carrying their results", async () => {
    await script({
      replies: [
        { text: "answer", toolCalls: [{ arguments: { key: "DEMO-12" }, name: "read_issue" }] },
        { text: "next conversation" },
      ],
    });
    const asked = await streamed(await chat.call("/chat/completions", completion({})));
    expect(asked.finish).toBe("tool_calls");
    expect(asked.content).toBe("");
    expect(asked.calls).toHaveLength(1);
    expect(asked.calls[0]).toMatchObject({ arguments: '{"key":"DEMO-12"}', name: "read_issue" });
    const answered = await streamed(
      await chat.call(
        "/chat/completions",
        completion({
          messages: [
            { content: "q", role: "user" },
            {
              role: "assistant",
              tool_calls: [{ function: asked.calls[0], id: asked.calls[0].id }],
            },
            { content: "{}", role: "tool", tool_call_id: asked.calls[0].id },
          ],
        })
      )
    );
    expect(answered.content).toBe("answer");
    const next = await streamed(await chat.call("/chat/completions", completion({})));
    expect(next.content).toBe("next conversation");
  });

  test("a status reply refuses with it, and a caller that leaves a slow stream is counted", async () => {
    await script({ replies: [{ status: 400 }], default: { delayMs: 200, text: "a slow reply" } });
    expect((await chat.call("/chat/completions", completion({}))).status).toBe(400);
    const leaving = new AbortController();
    const response = await chat.call("/chat/completions", {
      ...completion({}),
      signal: leaving.signal,
    });
    const reader = response.body?.getReader();
    await reader?.read();
    leaving.abort();
    await reader?.cancel().catch(() => undefined);
    // The fake counts the abandon in its own process, when its stream sees the connection go, so
    // there is no event here to await: the count is read until it moves, bounded.
    const deadline = Date.now() + 5000;
    while ((await calls()).abandoned === 0 && Date.now() < deadline) await Bun.sleep(50);
    expect(await calls()).toMatchObject({ abandoned: 1, calls: 2 });
  });
});

describe("the fake decision model", () => {
  let decision: Fake;

  beforeAll(async () => {
    decision = await startFake(
      "fake-decision-model.ts",
      "fakeDecisionModel",
      /fake decision model listener on 127\.0\.0\.1:(\d+)\n/,
      files.decisionToken
    );
  });

  afterAll(async () => {
    await decision?.stop();
  });

  const script = async (value: DecisionScript) => {
    expect((await decision.call("/__fixture/reset", { method: "PUT" })).status).toBe(200);
    const put = await decision.call("/__fixture/script", {
      body: JSON.stringify(value),
      method: "PUT",
    });
    expect(put.status).toBe(200);
  };
  const choice = (...names: string[]) => ({
    criteria: Object.fromEntries(names.map((name) => [name, `${name} means this`])),
    type: "choice",
  });
  const ask = (bearer?: string | null) => ({
    bearer,
    body: JSON.stringify({
      model: "the-chosen-model",
      questions: {
        action: choice("dispatch_claim", "dispatch_follow", "none"),
        context: choice("page", "named", "none"),
        display: choice("chart", "prose"),
      },
      state: { page: { path: "/" }, words: "claim DEMO-12" },
    }),
    headers: { "Content-Type": "application/json" },
    method: "POST",
  });

  test("answers each question System One's way, scripted or not", async () => {
    await script({
      replies: [
        {
          action: {
            choice: "dispatch_claim",
            confidence: 0.7,
            probabilities: { dispatch_claim: 0.7, dispatch_follow: 0.2, none: 0.1 },
          },
          display: { choice: "chart", confidence: 0.4 },
        },
      ],
    });
    const answered = await decision.call("/v1/systemone", ask());
    expect(answered.status).toBe(200);
    const body = await answered.json();
    expect(body.model).toBe("the-chosen-model");
    expect(body.usage.input_tokens).toBeGreaterThan(0);
    expect(body.answers.action).toEqual({
      choice: "dispatch_claim",
      confidence: 0.7,
      probabilities: { dispatch_claim: 0.7, dispatch_follow: 0.2, none: 0.1 },
      type: "choice",
    });
    expect(body.answers.display.choice).toBe("chart");
    expect(body.answers.display.probabilities.chart).toBe(0.4);
    expect(body.answers.display.probabilities.prose).toBeCloseTo(0.6);
    expect(body.answers.context).toMatchObject({ choice: "none", type: "choice" });
    // The list is spent, so the next call is answered as nothing scripted.
    const unscripted = await (await decision.call("/v1/systemone", ask())).json();
    expect(unscripted.answers.action.choice).toBe("none");
    expect(unscripted.answers.display.choice).toBe("prose");
  });

  test("a choice the call did not offer is refused naming the offered ones", async () => {
    await script({ default: { action: { choice: "dispatch_handoff", confidence: 0.9 } } });
    const refused = await decision.call("/v1/systemone", ask());
    expect(refused.status).toBe(422);
    expect((await refused.json()).error).toContain("dispatch_claim, dispatch_follow, none");
  });

  test("counts the calls it answered and refuses another bearer", async () => {
    await script({ default: { status: 503 } });
    expect((await decision.call("/v1/systemone", ask())).status).toBe(503);
    expect((await decision.call("/v1/systemone", ask("Bearer wrong"))).status).toBe(401);
    const counted = (await (await decision.call("/__fixture/calls")).json()) as FakeDecisionCalls;
    expect(counted).toMatchObject({ calls: 1, unauthorized: 1 });
    expect(counted.requests).toHaveLength(1);
  });
});
