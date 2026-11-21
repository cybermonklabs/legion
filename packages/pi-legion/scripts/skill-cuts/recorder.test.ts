import { afterEach, expect, test } from "bun:test";
import { responseMessage, streamedMessage, streamedResponse, toolCalls } from "./gateway";
import {
  END_TURN_REPLY,
  END_TURN_RESPONSE,
  messageSse,
  type Recorder,
  responseSse,
  sseResponse,
  startRecorder,
} from "./recorder";

// The recorder (recorder.ts): what it keeps of a request and what it answers. The event streams it
// writes are read back with gateway.ts's `streamedMessage` and `streamedResponse`, the readers
// every reply of the runner passes through.
const open: Recorder[] = [];
afterEach(async () => {
  for (const recorder of open.splice(0)) await recorder.close();
});

/** A recorder kept until the row ends. */
function recorder(...args: Parameters<typeof startRecorder>): Recorder {
  const started = startRecorder(...args);
  open.push(started);
  return started;
}

test("a request's body is kept byte for byte, and no auth header nor any header naming a key, a token, a team or a user", async () => {
  const stand = recorder();
  const body = '{"model":"m",  "messages":[]}\n';
  const answer = await fetch(`${stand.url}/v1/messages?beta=true`, {
    method: "POST",
    headers: {
      authorization: "Bearer made-up",
      "x-api-key": "made-up",
      "x-litellm-key-alias": "made-up",
      "x-team-id": "made-up",
      "x-user-id": "made-up",
      "x-session-token": "made-up",
      cookie: "a=b",
      "anthropic-version": "2023-06-01",
      "anthropic-beta": "one,two",
      "content-type": "application/json",
    },
    body,
  });
  expect(answer.status).toBe(200);
  expect(answer.headers.get("content-type")).toBe("text/event-stream");
  expect(await answer.text()).toBe(END_TURN_REPLY);
  expect(stand.requests).toHaveLength(1);
  const [kept] = stand.requests;
  expect(kept?.n).toBe(1);
  expect(kept?.method).toBe("POST");
  expect(kept?.path).toBe("/v1/messages?beta=true");
  expect(kept?.body).toBe(body);
  expect(kept?.headers).toMatchObject({
    "anthropic-version": "2023-06-01",
    "anthropic-beta": "one,two",
    "content-type": "application/json",
  });
  const left = ["authorization", "x-api-key", "x-litellm-key-alias", "x-team-id", "x-user-id"];
  for (const name of [...left, "x-session-token", "cookie"]) {
    expect(Object.keys(kept?.headers ?? {})).not.toContain(name);
  }
});

test("a route's answer stands in for the recorder's own, the recorder's own is in the API of the path, and anything but a model request is not found", async () => {
  const stand = recorder({
    route: (request) =>
      request.body.includes("special") ? new Response("routed", { status: 202 }) : undefined,
  });
  const post = (body: string, route = "/v1/messages") =>
    fetch(`${stand.url}${route}`, { method: "POST", body });
  expect(await (await post("special")).text()).toBe("routed");
  expect(await (await post("plain")).text()).toBe(END_TURN_REPLY);
  const responses = await post("plain", "/v1/responses");
  expect(responses.headers.get("content-type")).toBe("text/event-stream");
  expect(await responses.text()).toBe(END_TURN_RESPONSE);
  expect((await fetch(`${stand.url}/v1/models`)).status).toBe(404);
  expect(stand.requests.map((request) => [request.n, request.method, request.body])).toEqual([
    [1, "POST", "special"],
    [2, "POST", "plain"],
    [3, "POST", "plain"],
    [4, "GET", ""],
  ]);
});

test("the event streams it writes read back as the messages they were written from", async () => {
  expect(streamedMessage(END_TURN_REPLY)).toMatchObject({
    role: "assistant",
    content: [{ type: "text", text: "Recorded." }],
    stop_reason: "end_turn",
    usage: { output_tokens: 3 },
  });
  const sse = messageSse({
    id: "msg_1",
    model: "claude-sonnet-5-5",
    content: [
      { type: "thinking", thinking: "First the list.", signature: "sig-1" },
      { type: "text", text: "Reading it." },
      { type: "tool_use", id: "toolu_1", name: "read", input: { path: "skill://alpha" } },
    ],
    stopReason: "tool_use",
    inputTokens: 40,
    outputTokens: 12,
  });
  expect(streamedMessage(sse)).toMatchObject({
    id: "msg_1",
    model: "claude-sonnet-5-5",
    content: [
      { type: "thinking", thinking: "First the list.", signature: "sig-1" },
      { type: "text", text: "Reading it." },
      { type: "tool_use", id: "toolu_1", name: "read", input: { path: "skill://alpha" } },
    ],
    stop_reason: "tool_use",
    usage: { input_tokens: 40, output_tokens: 12 },
  });
  const response = sseResponse(sse, "req_1");
  expect(response.headers.get("request-id")).toBe("req_1");
  expect(await response.text()).toBe(sse);
});

test("the Responses streams it writes read back as the responses they were written from, in the F9 event shapes", () => {
  expect(responseMessage(streamedResponse(END_TURN_RESPONSE))).toMatchObject({
    model: "recorder",
    content: [
      { type: "message", role: "assistant", content: [{ type: "output_text", text: "Recorded." }] },
    ],
    stop_reason: "completed",
    usage: { input_tokens: 10, output_tokens: 3 },
  });
  const sse = responseSse({
    id: "resp_1",
    model: "gpt-6.1-sol",
    output: [
      { type: "reasoning", encryptedContent: "made-up-encrypted-content" },
      { type: "message", text: "Reading it." },
      {
        type: "function_call",
        callId: "call_1",
        name: "read",
        arguments: { path: "skill://alpha" },
      },
    ],
    inputTokens: 40,
    outputTokens: 12,
  });
  const message = responseMessage(streamedResponse(sse));
  expect(message).toMatchObject({
    id: "resp_1",
    model: "gpt-6.1-sol",
    content: [
      { type: "reasoning", encrypted_content: "made-up-encrypted-content" },
      { type: "message", content: [{ type: "output_text", text: "Reading it." }] },
      { type: "function_call", call_id: "call_1", name: "read" },
    ],
    usage: { input_tokens: 40, output_tokens: 12 },
  });
  expect(toolCalls(message.content)).toEqual([
    { id: "call_1", name: "read", input: { path: "skill://alpha" } },
  ]);
  // As the gateway's stream (F9): data lines only, numbered in order, closed by [DONE].
  const lines = sse.split("\n\n").filter((line) => line !== "");
  expect(lines.every((line) => line.startsWith("data: "))).toBe(true);
  expect(lines.at(-1)).toBe("data: [DONE]");
  const events = lines.slice(0, -1).map((line) => JSON.parse(line.slice("data: ".length)));
  expect(events.map((event) => event.sequence_number)).toEqual(events.map((_, index) => index));
  expect(events.map((event) => event.type)).toEqual([
    "response.created",
    "response.in_progress",
    "response.output_item.added",
    "response.output_item.done",
    "response.output_item.added",
    "response.content_part.added",
    "response.output_text.delta",
    "response.output_text.done",
    "response.content_part.done",
    "response.output_item.done",
    "response.output_item.added",
    "response.function_call_arguments.delta",
    "response.function_call_arguments.done",
    "response.output_item.done",
    "response.completed",
  ]);
});
