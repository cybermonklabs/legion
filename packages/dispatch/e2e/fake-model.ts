import { harnessPorts } from "./harness-ports";

// The stand-in model (INFRA-3231): the model service every model group of the server calls, since
// `e2e/run-server.sh` hands this listener's URL to the server as DISPATCH_TEST_MODEL_URL under
// DISPATCH_TEST_HOOKS=1. It answers both protocols a group speaks, over plain HTTP: Anthropic
// Messages (`POST /v1/messages`) and OpenAI Chat Completions (`POST <any path>/chat/completions`,
// which an Azure AI Foundry chat deployment speaks), each whole or streamed as the request asks.
// The server sends it no credential, and it checks none. Each request names its group's
// deployment as its model, so a row tells the reviewer's calls from Rewrite and Define's and the
// records job's by that name.
//
// It is the browser harness's counterpart of `internal/dispatch/modeltest`, and answers the same
// way: from the rules a row scripts, the first rule that matches a request answering it, and an
// error where none does. A row scripts it through `e2e/fake-model-helpers.ts`:
// `PUT /__fixture/rules` replaces the rules, `GET /__fixture/requests` answers every request since
// the last reset, `PUT /__fixture/requests` forgets them, and `PUT /__fixture/reset` clears both,
// which `e2e/seed.ts` does before every row.

/** The protocol a request came in: Anthropic Messages, or OpenAI Chat Completions. */
export type FakeModelProtocol = "anthropic" | "chat";

/** What the stand-in answers one request with. */
export interface FakeModelReply {
  /** The model's text; empty where unset. */
  readonly text?: string;
  /** Set and not 200, the protocol's error object is answered with this status, its message the
   *  text, in place of a reply. */
  readonly status?: number;
  /** How long the stand-in waits before it answers. */
  readonly delayMs?: number;
}

/** A rule answers the requests it matches with its reply. A request is matched by the first rule,
 *  in the order given, whose fields that are set all hold. */
export interface FakeModelRule {
  /** Matches a request that names it as its model: a group's deployment. */
  readonly model?: string;
  /** Matches a request whose text (`FakeModelRequest.text`) holds it. */
  readonly contains?: string;
  /** Uses the rule up on the first request it answers, so rules given with it answer the next
   *  requests in turn. */
  readonly once?: boolean;
  readonly reply: FakeModelReply;
}

/** One request the stand-in got. */
export interface FakeModelRequest {
  readonly protocol: FakeModelProtocol;
  readonly path: string;
  /** The URL's query, as it arrived, without its `?`. */
  readonly query: string;
  /** The model the request names. */
  readonly model: string;
  /** The text of every message the request carries, its system prompt first, joined by
   *  newlines. */
  readonly text: string;
  readonly stream: boolean;
  /** The request's JSON body. */
  readonly body: unknown;
}

/** What `GET /__fixture/requests` answers: every request since the last reset, oldest first. */
export interface FakeModelRequests {
  readonly requests: readonly FakeModelRequest[];
}

/** The part of either protocol's request the stand-in reads. */
interface WireRequest {
  readonly model?: string;
  readonly stream?: boolean;
  readonly system?: unknown;
  readonly messages?: readonly { readonly content?: unknown }[];
  readonly stream_options?: { readonly include_usage?: boolean };
}

/** The error a request no rule answers gets. */
const unscripted = "the stand-in model has no reply scripted for this request";

/** Every piece of fixture state this listener holds; `PUT /__fixture/reset` replaces it whole. */
interface FakeModelState {
  rules: FakeModelRule[];
  requests: FakeModelRequest[];
}

function emptyState(): FakeModelState {
  return { requests: [], rules: [] };
}

let state = emptyState();

/** The text a system prompt or a message's content carries: the string itself, or the text of
 *  each of its blocks or parts, a tool result's content included. */
function textsOf(content: unknown): string[] {
  if (typeof content === "string") return [content];
  if (Array.isArray(content)) return content.flatMap(textsOf);
  if (typeof content === "object" && content !== null) {
    const part = content as { readonly text?: unknown; readonly content?: unknown };
    return typeof part.text === "string" ? [part.text] : textsOf(part.content);
  }
  return [];
}

/** A rough count of a text's tokens, four characters a token, for a reply's usage. */
function tokens(text: string): number {
  return Math.max(Math.floor([...text].length / 4), 1);
}

/** The rule that answers `request`, using up a `once` rule; undefined where none does. */
function answer(request: FakeModelRequest): FakeModelReply | undefined {
  const index = state.rules.findIndex(
    (rule) =>
      (rule.model === undefined || rule.model === request.model) &&
      (rule.contains === undefined || request.text.includes(rule.contains))
  );
  if (index === -1) return undefined;
  const rule = state.rules[index];
  if (rule.once) state.rules.splice(index, 1);
  return rule.reply;
}

function isRule(value: unknown): value is FakeModelRule {
  if (typeof value !== "object" || value === null) return false;
  const rule = value as Record<string, unknown>;
  return (
    typeof rule.reply === "object" &&
    rule.reply !== null &&
    (rule.model === undefined || typeof rule.model === "string") &&
    (rule.contains === undefined || typeof rule.contains === "string") &&
    (rule.once === undefined || typeof rule.once === "boolean")
  );
}

/** One server-sent event, named where `name` is given. */
function event(name: string | undefined, data: unknown): string {
  return `${name === undefined ? "" : `event: ${name}\n`}data: ${JSON.stringify(data)}\n\n`;
}

function errorResponse(protocol: FakeModelProtocol, status: number, message: string): Response {
  const body =
    protocol === "anthropic"
      ? { error: { message, type: "api_error" }, type: "error" }
      : { error: { message, type: "server_error" } };
  return Response.json(body, { status });
}

/** The headers of a streamed reply. */
const eventStream = {
  headers: { "Cache-Control": "no-cache", "Content-Type": "text/event-stream" },
};

function anthropicResponse(request: FakeModelRequest, text: string): Response {
  const usage = { input_tokens: tokens(request.text), output_tokens: tokens(text) };
  const message = {
    id: "msg_standin",
    model: request.model,
    role: "assistant",
    stop_sequence: null,
    type: "message",
  };
  if (!request.stream) {
    return Response.json({
      ...message,
      content: [{ text, type: "text" }],
      stop_reason: "end_turn",
      usage,
    });
  }
  return new Response(
    event("message_start", {
      message: {
        ...message,
        content: [],
        stop_reason: null,
        usage: { input_tokens: usage.input_tokens, output_tokens: 0 },
      },
      type: "message_start",
    }) +
      event("content_block_start", {
        content_block: { text: "", type: "text" },
        index: 0,
        type: "content_block_start",
      }) +
      event("content_block_delta", {
        delta: { text, type: "text_delta" },
        index: 0,
        type: "content_block_delta",
      }) +
      event("content_block_stop", { index: 0, type: "content_block_stop" }) +
      event("message_delta", {
        delta: { stop_reason: "end_turn", stop_sequence: null },
        type: "message_delta",
        usage,
      }) +
      event("message_stop", { type: "message_stop" }),
    eventStream
  );
}

function chatResponse(request: FakeModelRequest, text: string, includeUsage: boolean): Response {
  const usage = {
    completion_tokens: tokens(text),
    prompt_tokens: tokens(request.text),
    total_tokens: tokens(request.text) + tokens(text),
  };
  if (!request.stream) {
    return Response.json({
      choices: [{ finish_reason: "stop", index: 0, message: { content: text, role: "assistant" } }],
      created: 1,
      id: "chatcmpl-standin",
      model: request.model,
      object: "chat.completion",
      usage,
    });
  }
  const chunk = (choices: unknown[], chunkUsage: unknown) =>
    event(undefined, {
      choices,
      created: 1,
      id: "chatcmpl-standin",
      model: request.model,
      object: "chat.completion.chunk",
      usage: chunkUsage,
    });
  return new Response(
    chunk([{ delta: { content: text, role: "assistant" }, finish_reason: null, index: 0 }], null) +
      chunk([{ delta: {}, finish_reason: "stop", index: 0 }], null) +
      (includeUsage ? chunk([], usage) : "") +
      "data: [DONE]\n\n",
    eventStream
  );
}

/** Answers one call of either protocol, recording it. */
async function serveModel(
  protocol: FakeModelProtocol,
  url: URL,
  incoming: Request
): Promise<Response> {
  let wire: WireRequest;
  try {
    wire = (await incoming.json()) as WireRequest;
  } catch (error) {
    return errorResponse(protocol, 400, `the request is no JSON object: ${String(error)}`);
  }
  const request: FakeModelRequest = {
    body: wire,
    model: wire.model ?? "",
    path: url.pathname,
    protocol,
    query: url.search.replace(/^\?/, ""),
    stream: wire.stream === true,
    text: [
      ...textsOf(wire.system),
      ...(wire.messages ?? []).flatMap((message) => textsOf(message.content)),
    ].join("\n"),
  };
  state.requests.push(request);
  const reply = answer(request);
  if (reply?.delayMs) await Bun.sleep(reply.delayMs);
  if (reply === undefined) return errorResponse(protocol, 500, unscripted);
  if (reply.status !== undefined && reply.status !== 200) {
    return errorResponse(protocol, reply.status, reply.text ?? "");
  }
  return protocol === "anthropic"
    ? anthropicResponse(request, reply.text ?? "")
    : chatResponse(request, reply.text ?? "", wire.stream_options?.include_usage === true);
}

// FAKE_MODEL_PORT, or with it unset port 0, so the OS chooses; the line at the end reports the
// port bound, which e2e/harness-start.ts reads.
const server = Bun.serve({
  hostname: "127.0.0.1",
  port: harnessPorts().fakeModel.port,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "POST" && url.pathname === "/v1/messages") {
      return serveModel("anthropic", url, request);
    }
    if (request.method === "POST" && url.pathname.endsWith("/chat/completions")) {
      return serveModel("chat", url, request);
    }
    if (request.method === "GET" && url.pathname === "/__fixture/requests") {
      const body: FakeModelRequests = { requests: state.requests };
      return Response.json(body);
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/rules") {
      const rules: unknown = await request.json();
      if (!Array.isArray(rules) || !rules.every(isRule)) {
        return Response.json(
          { message: "the rules are an array of {model?, contains?, once?, reply}" },
          { status: 400 }
        );
      }
      state = { ...state, rules: [...rules] };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/requests") {
      // The requests alone, so a row that made the server call a model while it was setting its
      // fixture up can start counting from zero without scripting its rules again.
      state = { ...state, requests: [] };
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/reset") {
      state = emptyState();
      return Response.json({ ok: true });
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return Response.json({ message: "not found" }, { status: 404 });
  },
});

console.log(`fake model listener on 127.0.0.1:${server.port}`);
