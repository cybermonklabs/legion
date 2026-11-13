import { fakeModelServing, modelsDirVariable } from "./harness-models";
import { harnessPorts, stackValue } from "./harness-ports";

// The writing model Dispatch answers the command bar through, as a deployment's
// `DISPATCH_ANSWERER=endpoint` reaches one: `POST {DISPATCH_CHAT_ENDPOINT}/chat/completions`,
// OpenAI's chat-completions shape, streamed, checking the token the file DISPATCH_CHAT_TOKEN_FILE
// names. The server reaches it under the models lane (`e2e/harness-models.ts`), over https with the
// certificate the harness made for the stack, so the call it makes is the call a deployment
// makes - agentloop's openai-chat provider, its route, its sign-in, its reading of the stream and
// of a structured reply - and nothing in the server knows this is a fake.
//
// It is the browser harness's counterpart of `packages/envoy/going/goingtest/goingtest.go`, and
// speaks what that fake speaks: a role frame, the content in frames of a few characters, then
// each tool call with its arguments spread over frames, then the turn's finish_reason and
// `[DONE]`. A row scripts what it answers (`PUT /__fixture/script`): a list of replies taken one
// per conversation in order, then the default reply, and with neither a 500 that says so. A reply
// is a Final object (the answer, a block and the offered actions' arguments, which a call carrying
// `response_format` gets as the JSON object going's Final schema describes, answer first, and a
// call carrying none gets as the answer alone in prose), plain prose (which a structured call
// reads as a reply that does not fit the schema), tool calls the model makes before its answer, a
// 400 to every call carrying `response_format` (a service with no structured output), or any
// status to every call. A reply with both tool calls and an answer serves the tool calls first,
// and its answer to the call that carries their results, so one reply is one conversation
// whatever else the server calls in between. `delayMs` is the pause between frames (the slow
// mode a cancellation row reads), and `chunk` how many characters a frame carries.
//
// It counts what it served (`GET /__fixture/calls`): every call, the structured ones, the replies
// the caller went away from before `[DONE]` (abandoned, which is what a cancelled run looks like
// from here), the calls refused for their bearer, and the last bodies it received.

/** The Final object of a reply. `spec` and `arguments` are JSON text on the wire; an object given
 *  here is written as its JSON. A block's kind defaults to `none`. */
export interface ChatFinal {
  readonly answer: string;
  readonly block?: {
    readonly kind: string;
    readonly title?: string;
    readonly spec?: unknown;
  };
  readonly arguments?: unknown;
}

/** One tool call the model makes. `arguments` is JSON text on the wire; an object given here is
 *  written as its JSON. */
export interface ChatToolCall {
  readonly name: string;
  readonly arguments: unknown;
}

/** What the model answers one conversation with. */
export interface ChatReply {
  readonly final?: ChatFinal;
  /** Prose, streamed as the content whatever the call asked for. */
  readonly text?: string;
  /** Calls made before the answer: served to the conversation's first call, and the answer to
   *  the call that carries their results. */
  readonly toolCalls?: readonly ChatToolCall[];
  /** A 400 to every call carrying `response_format`, which leaves the reply in place for the
   *  call without it that the server falls back to; a call carrying none is answered. */
  readonly refuseStructured?: boolean;
  /** Answered to the call that takes the reply, with the service's error body, instead of a
   *  completion: as the default reply, to every call. */
  readonly status?: number;
  /** Characters per frame; 8 when unset. */
  readonly chunk?: number;
  /** Milliseconds between frames. */
  readonly delayMs?: number;
}

/** What `PUT /__fixture/script` takes: the replies, one per conversation in order, and the reply
 *  every conversation after them gets. */
export interface ChatScript {
  readonly replies?: readonly ChatReply[];
  readonly default?: ChatReply;
}

/** A call's body as the server sent it. */
export interface ChatRequestBody {
  readonly model?: string;
  readonly stream?: boolean;
  readonly messages?: readonly {
    readonly role: string;
    readonly content?: unknown;
    readonly tool_call_id?: string;
    readonly tool_calls?: readonly {
      readonly id: string;
      readonly function: { readonly name: string; readonly arguments: string };
    }[];
  }[];
  readonly tools?: readonly { readonly function: { readonly name: string } }[];
  readonly response_format?: unknown;
  readonly max_tokens?: number;
}

/** What `GET /__fixture/calls` answers, since the last reset. */
export interface FakeChatCalls {
  /** Every call to the completions route that presented the bearer, refused ones included. */
  readonly calls: number;
  /** Those of them that carried `response_format`. */
  readonly structured: number;
  /** Replies the caller went away from before the model finished writing them. */
  readonly abandoned: number;
  /** Calls refused 401 for their bearer. */
  readonly unauthorized: number;
  /** The last bodies received, oldest first. */
  readonly requests: readonly ChatRequestBody[];
}

/** How many bodies `requests` keeps. */
const keptRequests = 20;
const defaultChunk = 8;

interface FakeChatState {
  calls: number;
  structured: number;
  abandoned: number;
  unauthorized: number;
  requests: ChatRequestBody[];
  /** The scripted replies not yet taken. */
  queue: ChatReply[];
  fallback: ChatReply | undefined;
  /** The reply each served tool call belongs to, by the call's id, so the call carrying its
   *  result is answered from that reply. */
  pending: Map<string, ChatReply>;
  /** Ids handed out, so every tool call this listener makes has one of its own. */
  ids: number;
}

function emptyState(): FakeChatState {
  return {
    abandoned: 0,
    calls: 0,
    fallback: undefined,
    ids: 0,
    pending: new Map(),
    queue: [],
    requests: [],
    structured: 0,
    unauthorized: 0,
  };
}

let state = emptyState();

const serving = fakeModelServing(
  stackValue(process.env, modelsDirVariable),
  "chatToken",
  "the fake chat model"
);

/** `value` as the JSON text the wire carries: a string as it is, anything else as its JSON. */
function jsonText(value: unknown, empty: string): string {
  if (value === undefined) return empty;
  return typeof value === "string" ? value : JSON.stringify(value);
}

/** The content a reply's answer turn writes: the Final object or its answer, as the call asked
 *  for it, or the prose where the reply names no Final. Written field by field, so the answer is
 *  the object's first property as the schema orders it and streams first. */
function content(reply: ChatReply, structured: boolean): string {
  if (reply.final === undefined) return reply.text ?? "";
  if (!structured) return reply.final.answer;
  const block = reply.final.block;
  return (
    `{"answer":${JSON.stringify(reply.final.answer)},` +
    `"block":{"kind":${JSON.stringify(block?.kind ?? "none")},` +
    `"title":${JSON.stringify(block?.title ?? "")},` +
    `"spec":${JSON.stringify(jsonText(block?.spec, ""))}},` +
    `"arguments":${JSON.stringify(jsonText(reply.final.arguments, "{}"))}}`
  );
}

/** `text` in runs of at most `size` characters, so a frame never splits a character. */
function pieces(text: string, size: number): string[] {
  const characters = Array.from(text);
  const out: string[] = [];
  for (let start = 0; start < characters.length; start += size) {
    out.push(characters.slice(start, start + size).join(""));
  }
  return out;
}

/** The service's error body, as goingtest's `refuse` writes it. */
function refuse(status: number, message: string): Response {
  return Response.json(
    { error: { code: "simulated_failure", message, type: "invalid_request_error" } },
    { status }
  );
}

/** One streamed turn: each frame a `data:` event, written `delayMs` apart, then `[DONE]`. A caller
 *  that goes away before `[DONE]` is counted as abandoned, once, on the counts of the row the call
 *  arrived in (`owner`), whenever it goes, and nothing more is written. */
function stream(
  request: Request,
  owner: FakeChatState,
  frames: readonly unknown[],
  delayMs: number
): Response {
  const encoder = new TextEncoder();
  let done = false;
  let counted = false;
  const abandon = () => {
    if (done || counted) return;
    counted = true;
    owner.abandoned++;
  };
  request.signal.addEventListener("abort", abandon, { once: true });
  const body = new ReadableStream<Uint8Array>({
    async start(controller) {
      try {
        for (const [index, frame] of frames.entries()) {
          if (index > 0 && delayMs > 0) await Bun.sleep(delayMs);
          if (request.signal.aborted || counted) return;
          controller.enqueue(encoder.encode(`data: ${JSON.stringify(frame)}\n\n`));
        }
        if (request.signal.aborted || counted) return;
        controller.enqueue(encoder.encode("data: [DONE]\n\n"));
        done = true;
        controller.close();
      } catch {
        // The stream was cancelled under a write: the caller went away.
        abandon();
      }
    },
    cancel() {
      abandon();
    },
  });
  return new Response(body, {
    headers: { "Cache-Control": "no-cache", "Content-Type": "text/event-stream" },
  });
}

/** A completion chunk carrying `choice`. */
function frame(choice: Record<string, unknown>): unknown {
  return {
    choices: [{ delta: {}, finish_reason: null, index: 0, ...choice }],
    created: 1,
    id: "chatcmpl-e2e-fake",
    model: "e2e-fake-chat",
    object: "chat.completion.chunk",
  };
}

/** The turn `reply` writes: its tool calls, or its content, then its finish_reason. */
function turn(reply: ChatReply, structured: boolean, toolTurn: boolean): unknown[] {
  const size = reply.chunk !== undefined && reply.chunk > 0 ? reply.chunk : defaultChunk;
  const frames: unknown[] = [frame({ delta: { role: "assistant" } })];
  if (toolTurn) {
    for (const [index, call] of (reply.toolCalls ?? []).entries()) {
      state.ids++;
      const id = `call_e2e_${state.ids}`;
      state.pending.set(id, reply);
      const [first = "", ...rest] = pieces(jsonText(call.arguments, "{}"), size);
      frames.push(
        frame({
          delta: {
            tool_calls: [
              { function: { arguments: first, name: call.name }, id, index, type: "function" },
            ],
          },
        })
      );
      for (const piece of rest) {
        frames.push(frame({ delta: { tool_calls: [{ function: { arguments: piece }, index }] } }));
      }
    }
    frames.push(frame({ finish_reason: "tool_calls" }));
    return frames;
  }
  for (const piece of pieces(content(reply, structured), size)) {
    frames.push(frame({ delta: { content: piece } }));
  }
  frames.push(frame({ finish_reason: "stop" }));
  return frames;
}

/** The reply a call is answered from, whether it answers with that reply's tool calls, and
 *  whether it was taken from the scripted list: the reply whose tool calls the call's last message
 *  answers, or the next scripted one, or the default. */
interface Chosen {
  readonly reply: ChatReply;
  readonly toolTurn: boolean;
  readonly queued: boolean;
}

function replyFor(body: ChatRequestBody): Chosen | undefined {
  const last = body.messages?.at(-1);
  if (last?.role === "tool" && last.tool_call_id !== undefined) {
    const continued = state.pending.get(last.tool_call_id);
    if (continued !== undefined) {
      for (const message of body.messages ?? []) {
        if (message.tool_call_id !== undefined) state.pending.delete(message.tool_call_id);
      }
      return { queued: false, reply: continued, toolTurn: false };
    }
  }
  const queued = state.queue.shift();
  const reply = queued ?? state.fallback;
  if (reply === undefined) return undefined;
  return { queued: queued !== undefined, reply, toolTurn: (reply.toolCalls?.length ?? 0) > 0 };
}

async function complete(request: Request): Promise<Response> {
  // The call counts on the row it arrived in: a reset between rows replaces `state`, and a stream
  // of this call abandoned after one still counts here, never on the next row.
  const owner = state;
  if (request.headers.get("authorization") !== serving.authorization) {
    owner.unauthorized++;
    return refuse(401, "a call whose bearer is not the one DISPATCH_CHAT_TOKEN_FILE holds");
  }
  const body = (await request.json()) as ChatRequestBody;
  const structured = body.response_format !== undefined && body.response_format !== null;
  owner.calls++;
  if (structured) owner.structured++;
  owner.requests = [...owner.requests, body].slice(-keptRequests);
  const chosen = replyFor(body);
  if (chosen === undefined) {
    return refuse(500, "the fake chat model has no reply scripted for this call");
  }
  const { queued, reply, toolTurn } = chosen;
  if (reply.status !== undefined) {
    return refuse(reply.status, "the model service refused this call");
  }
  if (reply.refuseStructured === true && structured) {
    // The prose call the server falls back to is this same conversation's, so it takes this
    // reply rather than the next.
    if (queued) state.queue.unshift(reply);
    return refuse(400, "response_format is not supported by this model");
  }
  return stream(request, owner, turn(reply, structured, toolTurn), reply.delayMs ?? 0);
}

// FAKE_CHAT_MODEL_PORT, or with it unset port 0, so the OS chooses; the line at the end reports
// the port bound, which e2e/harness-start.ts reads. No idle timeout, since a slow reply's pause
// between frames is the row's to choose.
const server = Bun.serve({
  hostname: "127.0.0.1",
  idleTimeout: 0,
  port: harnessPorts().fakeChatModel.port,
  tls: serving.tls,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method === "POST" && url.pathname.endsWith("/chat/completions")) {
      return complete(request);
    }
    if (request.method === "GET" && url.pathname === "/__fixture/calls") {
      const calls: FakeChatCalls = {
        abandoned: state.abandoned,
        calls: state.calls,
        requests: state.requests,
        structured: state.structured,
        unauthorized: state.unauthorized,
      };
      return Response.json(calls);
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/script") {
      const script = (await request.json()) as ChatScript;
      // In place, so a call already counting on this row's counts keeps counting on them.
      state.fallback = script.default;
      state.pending = new Map();
      state.queue = [...(script.replies ?? [])];
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

console.log(`fake chat model listener on 127.0.0.1:${server.port}`);
