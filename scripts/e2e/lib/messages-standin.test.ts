import { afterAll, beforeAll, expect, test } from "bun:test";
import { existsSync, readFileSync, writeFileSync } from "node:fs";
import { join } from "node:path";
import { temporaryDirectory } from "@legion/test-fixtures/temporary-directories";
import type { Subprocess } from "bun";

// The model stand-in as Codex CLI calls it, over OpenAI's Responses protocol: the preflight
// nats-callout-sessions.sh holds a Codex route to (lib/model-endpoint.sh's
// responses_key_preflight), a streamed turn, and a scripted stand-in's tool call and the turn that
// carries its output. Each stand-in runs as a proof runs it, a process of its own on the three
// arguments its header names.
const lib = import.meta.dir;
const dir = temporaryDirectory("messages-standin-test");
const key = "standin-test-key";
const model = "standin-codex";
const keyFile = join(dir, "key");

interface Standin {
  readonly base: string;
  readonly requests: string;
  readonly child: Subprocess;
}
const started: Subprocess[] = [];

/** Runs `entry` as a proof runs the stand-in, and waits for the port it reports. */
async function start(name: string, entry: string, ...rest: string[]): Promise<Standin> {
  const portFile = join(dir, `${name}.port`);
  const requests = join(dir, `${name}-requests.jsonl`);
  const child = Bun.spawn([process.execPath, entry, portFile, keyFile, requests, ...rest], {
    stdout: "ignore",
    stderr: "inherit",
  });
  started.push(child);
  // The stand-in is a process of its own whose one signal is the port file it writes, which a
  // proof polls the same way (lib/port-file.sh); no timer of this process's decides anything.
  for (let poll = 0; !existsSync(portFile); poll++) {
    if (poll > 400 || child.exitCode !== null) throw new Error(`${entry} reported no port`);
    await Bun.sleep(50);
  }
  return { base: `http://127.0.0.1:${readFileSync(portFile, "utf8").trim()}`, requests, child };
}

/** POST /v1/responses with the key, streamed as Codex asks, and the events it answered. */
async function turn(base: string, body: Record<string, unknown>) {
  const response = await fetch(`${base}/v1/responses`, {
    method: "POST",
    headers: { authorization: `Bearer ${key}`, "content-type": "application/json" },
    body: JSON.stringify({ model, stream: true, ...body }),
  });
  expect(response.status).toBe(200);
  expect(response.headers.get("content-type")).toBe("text/event-stream");
  return (await response.text())
    .split("\n\n")
    .filter((frame) => frame !== "")
    .map((frame) => {
      const [event, data] = frame.split("\n");
      const parsed = JSON.parse((data ?? "").replace(/^data: /, "")) as Record<string, unknown>;
      expect(event).toBe(`event: ${String(parsed.type)}`);
      return parsed;
    });
}

/** The output items of a turn's events. */
function items(events: readonly Record<string, unknown>[]): Record<string, unknown>[] {
  return events
    .filter((event) => event.type === "response.output_item.done")
    .map((event) => event.item as Record<string, unknown>);
}

let standin: Standin;
let scripted: Standin;
beforeAll(async () => {
  writeFileSync(keyFile, `${key}\n`, { mode: 0o600 });
  // A scripted stand-in, as lib/plugin-smoke-model.ts and its siblings are: one that calls the
  // offered exec_command, then answers with the tool result the next request carries.
  const module = join(dir, "scripted-model.ts");
  writeFileSync(
    module,
    `import { afterToolResult, runStandin, text, toolCall } from ${JSON.stringify(join(lib, "messages-standin.ts"))};
const call = (turn) => toolCall(turn, "exec_command", { cmd: "true" });
runStandin((turn) =>
  afterToolResult(turn)
    ? text(\`saw \${turn.messages.at(-1)?.results.join(",")} after \${turn.messages.at(-2)?.calls.map((each) => each.name).join(",")}\`)
    : call(turn) === undefined ? text("no exec_command offered") : [call(turn)]
);
`
  );
  [standin, scripted] = await Promise.all([
    start("plain", join(lib, "messages-standin.ts"), "ready", model),
    start("scripted", module),
  ]);
});
afterAll(async () => {
  for (const child of started) child.kill("SIGTERM");
  await Promise.all(started.map((child) => child.exited));
});

/** responses_key_preflight against BASE for MODEL, as nats-callout-sessions.sh calls it. */
function preflight(base: string, listed: string) {
  const run = Bun.spawnSync(
    [
      "bash",
      "-c",
      `set -Eeuo pipefail
fail() { echo "FAIL: $*"; exit 1; }
note() { echo "note: $*"; }
source "$1/model-endpoint.sh"
responses_key_preflight "$2" "$3" "$4/header" "the key file" "$5"`,
      "_",
      lib,
      base,
      keyFile,
      dir,
      listed,
    ],
    { stdout: "pipe", stderr: "pipe" }
  );
  return { exitCode: run.exitCode, output: run.stdout.toString() + run.stderr.toString() };
}

test("a Codex route's preflight finds the stand-in checks its key and lists the model it was named", () => {
  const passed = preflight(`${standin.base}/v1`, model);
  expect(passed.output).toBe(
    `note: the model endpoint ${standin.base}/v1 checks keys: GET /models with no key answered 401, and with the key in the key file lists ${model}\n`
  );
  expect(passed.exitCode).toBe(0);
  const refused = preflight(`${standin.base}/v1`, "another-model");
  expect(refused.output).toBe(
    `FAIL: the model endpoint ${standin.base}/v1 answers the key in the key file but lists no model another-model in GET /models\n`
  );
  expect(refused.exitCode).toBe(1);
});

test("a streamed Responses turn is answered with the reply as one assistant message, and recorded", async () => {
  const events = await turn(standin.base, {
    instructions: "You are Codex.",
    input: [
      { type: "message", role: "user", content: [{ type: "input_text", text: "hello there" }] },
    ],
  });
  expect(events.map((event) => event.type)).toEqual([
    "response.created",
    "response.output_item.done",
    "response.completed",
  ]);
  expect(items(events)).toEqual([
    {
      id: "msg_standin_1_message",
      type: "message",
      role: "assistant",
      status: "completed",
      content: [{ type: "output_text", text: "ready", annotations: [] }],
    },
  ]);
  const recorded = readFileSync(standin.requests, "utf8")
    .trim()
    .split("\n")
    .map((line) => JSON.parse(line) as Record<string, unknown>)
    .filter((line) => line.path === "/v1/responses");
  expect(recorded).toMatchObject([
    {
      method: "POST",
      model,
      keyed: true,
      text: "hello there",
      answer: [{ type: "text", text: "ready" }],
    },
  ]);
});

test("a Responses turn without the key is refused 401, as OpenAI refuses one", async () => {
  const response = await fetch(`${standin.base}/v1/responses`, {
    method: "POST",
    headers: { authorization: "Bearer another-key", "content-type": "application/json" },
    body: JSON.stringify({ model, stream: true, input: "hello" }),
  });
  expect(response.status).toBe(401);
  expect(await response.json()).toEqual({
    error: { message: "invalid api key", type: "invalid_request_error" },
  });
});

test("a scripted stand-in's tool call reaches Codex as a function call, and its output as the tool result", async () => {
  const tools = [
    {
      type: "function",
      name: "exec_command",
      parameters: { type: "object", properties: { cmd: { type: "string" } } },
    },
  ];
  const prompt = { type: "message", role: "user", content: [{ type: "input_text", text: "go" }] };
  const called = items(await turn(scripted.base, { tools, input: [prompt] }));
  expect(called).toEqual([
    {
      id: "fc_msg_standin_1_0",
      type: "function_call",
      call_id: "call_msg_standin_1_0",
      name: "exec_command",
      arguments: JSON.stringify({ cmd: "true" }),
    },
  ]);
  const answered = items(
    await turn(scripted.base, {
      tools,
      input: [
        prompt,
        ...called,
        { type: "function_call_output", call_id: "call_msg_standin_1_0", output: "exit 0" },
      ],
    })
  );
  expect(answered.map((item) => item.content)).toEqual([
    [{ type: "output_text", text: "saw exit 0 after exec_command", annotations: [] }],
  ]);
});
