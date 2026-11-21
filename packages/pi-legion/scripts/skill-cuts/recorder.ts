/**
 * A stand-in model endpoint on 127.0.0.1, of the Anthropic Messages API
 * and of OpenAI's Responses API: a host process pointed at it sends its model requests here, never
 * to the gateway. The recorder keeps each request, its body byte for byte and its headers less
 * every auth header and every header that names a key, a token, a secret, a team or a user, and
 * answers it: with what `route` returns, or, when `route` returns nothing, with a fixed
 * end-of-turn reply streamed as Server-Sent Events in the API its path names (END_TURN_REPLY to
 * `/v1/messages`, END_TURN_RESPONSE to `/v1/responses`). capture.ts records Oh My Pi's request
 * bodies with it, load.ts Claude Code's, and the tests build their stand-in gateways on it.
 *
 * `messageSse` writes a reply message as the event stream the Messages API sends for a request
 * that sets `stream` (the event shapes of the F5 replies): a `message_start`, each content block as
 * a start, one delta and a stop, a `message_delta` with the stop reason and the output tokens, and
 * a `message_stop`. It writes text, `tool_use` and `thinking` blocks. `responseSse` writes a
 * response as the event stream the gateway's Responses route sends (the event shapes of the F9
 * replies: `data` lines with no event names, each item added, its text or arguments as one delta
 * and done, then `response.completed` with the whole response, and `[DONE]`). It writes `message`,
 * `function_call` and `reasoning` items.
 */

/** A request the recorder took: its number from 1 in the order the requests arrived, method, path
 *  with its query, the headers it keeps, and the body as text. */
export interface RecordedRequest {
  readonly n: number;
  readonly method: string;
  readonly path: string;
  readonly headers: Readonly<Record<string, string>>;
  readonly body: string;
}

/** The answer to a request, or undefined for the recorder's own: END_TURN_REPLY to a POST to
 *  `/v1/messages`, END_TURN_RESPONSE to a POST to `/v1/responses`, a 404 to anything else. */
export type Route = (
  request: RecordedRequest
) => Response | undefined | Promise<Response | undefined>;

export interface Recorder {
  /** The base URL, `http://127.0.0.1:<port>`, with no trailing slash. */
  readonly url: string;
  /** Every request taken so far, in the order their bodies were read whole. */
  readonly requests: readonly RecordedRequest[];
  /** Stops the server; requests still open are ended. */
  close(): Promise<void>;
}

/** A header the recorder never keeps: an auth header, or one that names a key, a token, a secret,
 *  a team or a user (the gateway's `x-litellm-*` key alias and team headers among them). */
const LEFT_OUT_HEADER =
  /^(authorization|proxy-authorization|cookie|set-cookie)$|key|token|secret|auth|team|user/i;

/** The headers of `headers` the recorder keeps, by lower-case name. */
export function keptHeaders(headers: Headers): Record<string, string> {
  const kept: Record<string, string> = {};
  for (const [name, value] of headers) {
    if (!LEFT_OUT_HEADER.test(name)) kept[name.toLowerCase()] = value;
  }
  return kept;
}

/** A block of a reply message that `messageSse` writes. */
export type ReplyBlock =
  | { readonly type: "text"; readonly text: string }
  | {
      readonly type: "tool_use";
      readonly id: string;
      readonly name: string;
      readonly input: Readonly<Record<string, unknown>>;
    }
  | { readonly type: "thinking"; readonly thinking: string; readonly signature: string };

/** The reply message `messageSse` streams. */
export interface ReplyMessage {
  readonly id: string;
  readonly model: string;
  readonly content: readonly ReplyBlock[];
  readonly stopReason: "end_turn" | "tool_use" | "max_tokens";
  readonly inputTokens?: number;
  readonly outputTokens?: number;
}

/** One Server-Sent Event: its name and its data as one JSON line. */
function event(name: string, data: unknown): string {
  return `event: ${name}\ndata: ${JSON.stringify(data)}\n\n`;
}

/** `message` as the event stream the Messages API sends for a request that sets `stream`. */
export function messageSse(message: ReplyMessage): string {
  const events = [
    event("message_start", {
      type: "message_start",
      message: {
        id: message.id,
        type: "message",
        role: "assistant",
        model: message.model,
        content: [],
        stop_reason: null,
        stop_sequence: null,
        usage: {
          input_tokens: message.inputTokens ?? 10,
          cache_creation_input_tokens: 0,
          cache_read_input_tokens: 0,
          output_tokens: 1,
        },
      },
    }),
  ];
  message.content.forEach((block, index) => {
    const started =
      block.type === "text"
        ? { type: "text", text: "" }
        : block.type === "tool_use"
          ? { type: "tool_use", id: block.id, name: block.name, input: {} }
          : { type: "thinking", thinking: "", signature: "" };
    events.push(
      event("content_block_start", { type: "content_block_start", index, content_block: started })
    );
    const deltas =
      block.type === "text"
        ? [{ type: "text_delta", text: block.text }]
        : block.type === "tool_use"
          ? [{ type: "input_json_delta", partial_json: JSON.stringify(block.input) }]
          : [
              { type: "thinking_delta", thinking: block.thinking },
              { type: "signature_delta", signature: block.signature },
            ];
    for (const delta of deltas) {
      events.push(event("content_block_delta", { type: "content_block_delta", index, delta }));
    }
    events.push(event("content_block_stop", { type: "content_block_stop", index }));
  });
  events.push(
    event("message_delta", {
      type: "message_delta",
      delta: { stop_reason: message.stopReason, stop_sequence: null },
      usage: { output_tokens: message.outputTokens ?? 3 },
    }),
    event("message_stop", { type: "message_stop" })
  );
  return events.join("");
}

/** The recorder's own answer to a Messages request: one text block, `Recorded.`, ending the
 *  turn. */
export const END_TURN_REPLY = messageSse({
  id: "msg_recorder_end_turn",
  model: "recorder",
  content: [{ type: "text", text: "Recorded." }],
  stopReason: "end_turn",
});

/** An output item of a response that `responseSse` writes: a message's text, a function call
 *  with its arguments, or a reasoning item's encrypted content. */
export type ResponseItem =
  | { readonly type: "message"; readonly text: string }
  | {
      readonly type: "function_call";
      readonly callId: string;
      readonly name: string;
      readonly arguments: Readonly<Record<string, unknown>>;
    }
  | { readonly type: "reasoning"; readonly encryptedContent: string };

/** The response `responseSse` streams. */
export interface ResponseReply {
  readonly id: string;
  readonly model: string;
  readonly output: readonly ResponseItem[];
  readonly inputTokens?: number;
  readonly outputTokens?: number;
}

/** `item`, the `index`-th of response `id`, as the Responses API writes an output item whole. */
function wireItem(item: ResponseItem, index: number, id: string): Record<string, unknown> {
  const itemId = `${id}_${index}`;
  switch (item.type) {
    case "message":
      return {
        id: `msg_${itemId}`,
        type: "message",
        role: "assistant",
        status: "completed",
        content: [{ type: "output_text", text: item.text, annotations: [] }],
      };
    case "function_call":
      return {
        id: `fc_${itemId}`,
        type: "function_call",
        status: "completed",
        call_id: item.callId,
        name: item.name,
        arguments: JSON.stringify(item.arguments),
      };
    case "reasoning":
      return {
        id: `rs_${itemId}`,
        type: "reasoning",
        summary: [],
        encrypted_content: item.encryptedContent,
      };
  }
}

/** `response` as the event stream the gateway's Responses route sends for a request that sets
 *  `stream` (F9). */
export function responseSse(response: ResponseReply): string {
  const lines: string[] = [];
  const data = (value: Record<string, unknown>) => {
    lines.push(`data: ${JSON.stringify({ ...value, sequence_number: lines.length })}\n\n`);
  };
  const items = response.output.map((item, index) => wireItem(item, index, response.id));
  const head = {
    id: response.id,
    object: "response",
    model: response.model,
    status: "in_progress",
    output: [],
  };
  data({ type: "response.created", response: head });
  data({ type: "response.in_progress", response: head });
  response.output.forEach((item, index) => {
    const whole = items[index] ?? {};
    const at = { item_id: whole.id, output_index: index };
    if (item.type === "message") {
      const part = { type: "output_text", text: "", annotations: [] };
      data({
        type: "response.output_item.added",
        output_index: index,
        item: { ...whole, status: "in_progress", content: [] },
      });
      data({ type: "response.content_part.added", ...at, content_index: 0, part });
      data({ type: "response.output_text.delta", ...at, content_index: 0, delta: item.text });
      data({ type: "response.output_text.done", ...at, content_index: 0, text: item.text });
      data({
        type: "response.content_part.done",
        ...at,
        content_index: 0,
        part: { ...part, text: item.text },
      });
    } else if (item.type === "function_call") {
      data({
        type: "response.output_item.added",
        output_index: index,
        item: { ...whole, status: "in_progress", arguments: "" },
      });
      data({ type: "response.function_call_arguments.delta", ...at, delta: whole.arguments });
      data({ type: "response.function_call_arguments.done", ...at, arguments: whole.arguments });
    } else {
      data({ type: "response.output_item.added", output_index: index, item: whole });
    }
    data({ type: "response.output_item.done", output_index: index, item: whole });
  });
  const input = response.inputTokens ?? 10;
  const output = response.outputTokens ?? 3;
  data({
    type: "response.completed",
    response: {
      ...head,
      status: "completed",
      output: items,
      usage: {
        input_tokens: input,
        input_tokens_details: { cached_tokens: 0, cache_write_tokens: 0 },
        output_tokens: output,
        output_tokens_details: { reasoning_tokens: 0 },
        total_tokens: input + output,
      },
    },
  });
  return `${lines.join("")}data: [DONE]\n\n`;
}

/** The recorder's own answer to a Responses request: one message, `Recorded.`, ending the turn. */
export const END_TURN_RESPONSE = responseSse({
  id: "resp_recorder_end_turn",
  model: "recorder",
  output: [{ type: "message", text: "Recorded." }],
});

/** `sse` as the response a streamed request of either API gets. */
export function sseResponse(sse: string, requestId?: string): Response {
  const headers: Record<string, string> = { "content-type": "text/event-stream" };
  if (requestId !== undefined) headers["request-id"] = requestId;
  return new Response(sse, { headers });
}

/** Starts a recorder on 127.0.0.1 at a free port. */
export function startRecorder(options: { readonly route?: Route } = {}): Recorder {
  const requests: RecordedRequest[] = [];
  // Requests arrived so far: a request's number is taken before its body is read, so two
  // requests in flight at once never get the same one.
  let arrived = 0;
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    // A route may hold a request open for minutes (capture.ts holds Oh My Pi's turn until its
    // skill hints are written), so the connection is never ended for being idle.
    idleTimeout: 0,
    async fetch(request) {
      const url = new URL(request.url);
      arrived += 1;
      const recorded: RecordedRequest = {
        n: arrived,
        method: request.method,
        path: url.pathname + url.search,
        headers: keptHeaders(request.headers),
        body: request.method === "GET" || request.method === "HEAD" ? "" : await request.text(),
      };
      requests.push(recorded);
      const answer = await options.route?.(recorded);
      if (answer !== undefined) return answer;
      if (recorded.method === "POST" && url.pathname === "/v1/messages") {
        return sseResponse(END_TURN_REPLY, `req_recorder_${recorded.n}`);
      }
      if (recorded.method === "POST" && url.pathname === "/v1/responses") {
        return sseResponse(END_TURN_RESPONSE, `req_recorder_${recorded.n}`);
      }
      return new Response("not found", { status: 404 });
    },
  });
  return {
    url: `http://127.0.0.1:${server.port}`,
    requests,
    close: async () => {
      await server.stop(true);
    },
  };
}
