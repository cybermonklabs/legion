import { fakeModelServing, modelsDirVariable } from "./harness-models";
import { harnessPorts, stackValue } from "./harness-ports";

// The writing model Dispatch answers the command bar through, as a deployment's
// `DISPATCH_ANSWERER=endpoint` reaches one: `POST {DISPATCH_CHAT_ENDPOINT}/chat/completions`,
// OpenAI's chat-completions shape, streamed, checking the token the file DISPATCH_CHAT_TOKEN_FILE
// names. The server reaches it under the models lane (`e2e/harness-models.ts`), over https with the
// certificate the harness made for the stack, so the call it makes is the call a deployment
// makes - agentloop's openai-chat provider, its route, its sign-in, its reading of the stream and
// of a structured reply - and nothing in the server knows this is a fake. The server hands the
// same settings and the same certificate to its agent process (`envoy-dispatch agent`), whose
// runs call this fake with the same wire.
//
// It is the browser harness's counterpart of `packages/envoy/going/goingtest/goingtest.go` and
// `packages/envoy/going/agent/agenttest/agenttest.go`, and speaks what those fakes speak: a role
// frame, the content in frames of a few characters, then each tool call with its arguments spread
// over frames, then the turn's finish_reason and `[DONE]`. A row scripts what it answers
// (`PUT /__fixture/script`) in two parts.
//
// The agent's runs (`runs`): a run's every model call gives the person's words as the agent's loop
// writes them, `The person asks now:\n<words>\n` (going/agent's conversation, and the review of a
// plan in Auto-review), so each call is matched to the script of its words and answered with that
// script's next turn, then with its `afterwards` turn for every call after, and with neither a 500
// that says so. A call of a run whose words no script names is answered as every other call is,
// below. A turn is prose, tool calls (find_tools, call_tool, propose_plan or any other) after
// that prose, a stream with no end that writes until the caller goes away, or a status with its
// Retry-After (a 429, which the agent waits out and asks again, the next call taking the next
// turn). So a row scripts a run step by step, and a run continued after its plan is approved takes
// the turns after the plan's.
//
// Every other call (`replies`, `default`), which is the short answer the server streams as the
// person pauses: a list of replies taken one per conversation in order, then the default reply,
// and with neither a 500 that says so. A reply is a Final object (the answer and a block, which a
// call carrying `response_format` gets as the JSON object going's Final schema describes, answer
// first, and a call carrying none gets as the answer alone in prose), plain prose (which a
// structured call reads as a reply that does not fit the schema),
// tool calls the model makes before its answer, a 400 to every call carrying `response_format` (a
// service with no structured output), or any status to every call. A reply with both tool calls
// and an answer serves the tool calls first, and its answer to the call that carries their
// results, so one reply is one conversation whatever else the server calls in between. `delayMs`
// is the pause between frames (the slow mode a cancellation row reads), and `chunk` how many
// characters a frame carries.
//
// It counts what it served (`GET /__fixture/calls`): every call, the structured ones, the replies
// the caller went away from before `[DONE]` (abandoned, which is what a cancelled run looks like
// from here), the calls refused for their bearer, and the last bodies it received; and it records
// every call since the last reset (`received`): when it came, the words and the page of the run it
// belongs to, the tools it offered, the tool results it gave the model, the turn it was answered
// with, and when its caller went away.

/** The Final object of a reply. `spec` is JSON text on the wire; an object given here is written
 *  as its JSON. A block's kind defaults to `none`. */
export interface ChatFinal {
  readonly answer: string;
  readonly block?: {
    readonly kind: string;
    readonly title?: string;
    readonly spec?: unknown;
  };
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

/** What the model answers one model call of an agent run with. */
export interface ChatTurn {
  /** Prose, streamed first. A turn of prose and no tool call ends the run's turn. */
  readonly text?: string;
  /** The tool calls the model makes after its prose, each its own frames. */
  readonly toolCalls?: readonly ChatToolCall[];
  /** A stream that writes prose every 50 ms and never finishes, until the caller goes away. */
  readonly endless?: boolean;
  /** Answered instead of a completion, with the service's error body. */
  readonly status?: number;
  /** The `Retry-After` header of a `status` answer. */
  readonly retryAfter?: string;
  /** Characters per frame; 8 when unset. */
  readonly chunk?: number;
  /** Milliseconds between frames. */
  readonly delayMs?: number;
}

/** What one agent run's model calls are answered with: the run whose words are `words`. */
export interface ChatRunScript {
  /** The person's words, as the bar sent them. */
  readonly words: string;
  /** One turn per model call of the run, in order. */
  readonly turns: readonly ChatTurn[];
  /** The turn every call after `turns` gets; with none, such a call is answered 500. */
  readonly afterwards?: ChatTurn;
}

/** What `PUT /__fixture/script` takes: the agent runs, by their words; and for every other call
 *  the replies, one per conversation in order, and the reply every conversation after them gets. */
export interface ChatScript {
  readonly runs?: readonly ChatRunScript[];
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

/** One call the fake received, as `received` records it. */
export interface ChatCallRecord {
  /** When the call came, in epoch milliseconds. */
  readonly at: number;
  /** The person's words of the agent run the call belongs to; null for a call of no run. */
  readonly words: string | null;
  /** The page the run asks from, as the agent gives it the model: its JSON read back, or its text
   *  where that is no JSON; null where the run carries none. */
  readonly context: unknown;
  /** The tools the call offers the model, by name. */
  readonly tools: readonly string[];
  /** The text of each tool result the call gives the model, in order. */
  readonly toolResults: readonly string[];
  /** Which turn of its run's script answered it, from 0, a number past the turns being the
   *  `afterwards` turn; null for a call of no run. */
  turn: number | null;
  /** When the caller went away before the reply was whole, in epoch milliseconds; null while it
   *  has not. */
  left: number | null;
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
  /** Every call that presented the bearer, oldest first. */
  readonly received: readonly ChatCallRecord[];
}

/** How many bodies `requests` keeps. */
const keptRequests = 20;
const defaultChunk = 8;
/** The pause between two frames of an endless turn. */
const endlessGapMs = 50;

/** The line the agent's loop writes before the person's words, as agenttest's `Asked` reads it. */
const askedLine = "The person asks now:\n";
/** The line the agent's loop writes before the page a run carries; a colon and a line break end
 *  it, after the note that says the page is data. */
const pageLine = "\nThe page the person asks from, ";

interface RunState {
  readonly script: ChatRunScript;
  /** How many of the run's calls were answered. */
  taken: number;
}

interface FakeChatState {
  calls: number;
  structured: number;
  abandoned: number;
  unauthorized: number;
  requests: ChatRequestBody[];
  received: ChatCallRecord[];
  /** The scripted replies not yet taken. */
  queue: ChatReply[];
  fallback: ChatReply | undefined;
  /** The scripted runs, by their words. */
  runs: Map<string, RunState>;
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
    received: [],
    requests: [],
    runs: new Map(),
    structured: 0,
    unauthorized: 0,
  };
}

let state = emptyState();

/** Ends every endless stream still open, which a reset does: a run of the row before goes on no
 *  longer than its row. */
const endlessStreams = new Set<() => void>();

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
    `"spec":${JSON.stringify(jsonText(block?.spec, ""))}}}`
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
function refuse(status: number, message: string, retryAfter?: string): Response {
  return Response.json(
    { error: { code: "simulated_failure", message, type: "invalid_request_error" } },
    { headers: retryAfter === undefined ? {} : { "Retry-After": retryAfter }, status }
  );
}

/** The text a message's content carries: the string, or its parts' texts joined. */
function messageText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((part) =>
      part !== null && typeof part === "object" && "text" in part && typeof part.text === "string"
        ? part.text
        : ""
    )
    .join("\n");
}

/** The words and the page of the agent run a call belongs to, read from the first message the
 *  agent's loop writes; words null for a call of no run. */
function runOf(body: ChatRequestBody): {
  readonly words: string | null;
  readonly context: unknown;
} {
  const opening = messageText(body.messages?.find((message) => message.role === "user")?.content);
  const asked = opening.indexOf(askedLine);
  if (asked < 0) return { context: null, words: null };
  const from = asked + askedLine.length;
  const end = opening.indexOf("\n", from);
  const words = opening.slice(from, end < 0 ? undefined : end);
  const page = opening.indexOf(pageLine);
  if (page < 0) return { context: null, words };
  const header = opening.indexOf(":\n", page + pageLine.length);
  if (header < 0) return { context: null, words };
  const lineEnd = opening.indexOf("\n", header + 2);
  const line = opening.slice(header + 2, lineEnd < 0 ? undefined : lineEnd);
  try {
    return { context: JSON.parse(line), words };
  } catch {
    return { context: line, words };
  }
}

const encoder = new TextEncoder();

/** The event that ends a stream's turn. */
const DONE = "[DONE]";

/** One `data:` event of a stream: `frame`'s JSON, or `[DONE]`. */
function event(frame: unknown): Uint8Array {
  return encoder.encode(`data: ${frame === DONE ? DONE : JSON.stringify(frame)}\n\n`);
}

/** A response streaming `body`'s events, as the service streams a completion. */
function eventStream(body: ReadableStream<Uint8Array>): Response {
  return new Response(body, {
    headers: { "Cache-Control": "no-cache", "Content-Type": "text/event-stream" },
  });
}

/** One streamed turn: each frame a `data:` event, written `delayMs` apart, then `[DONE]`. A caller
 *  that goes away before `[DONE]` is counted as abandoned, once, on the counts of the row the call
 *  arrived in (`owner`), and on its record, whenever it goes, and nothing more is written. */
function stream(
  request: Request,
  owner: FakeChatState,
  record: ChatCallRecord,
  frames: readonly unknown[],
  delayMs: number
): Response {
  let done = false;
  let counted = false;
  const abandon = () => {
    if (done || counted) return;
    counted = true;
    owner.abandoned++;
    record.left = Date.now();
  };
  request.signal.addEventListener("abort", abandon, { once: true });
  const body = new ReadableStream<Uint8Array>({
    async start(controller) {
      try {
        for (const [index, frame] of frames.entries()) {
          if (index > 0 && delayMs > 0) await Bun.sleep(delayMs);
          if (request.signal.aborted || counted) return;
          controller.enqueue(event(frame));
        }
        if (request.signal.aborted || counted) return;
        controller.enqueue(event(DONE));
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
  return eventStream(body);
}

/** A turn that never ends: the role frame, then prose every `endlessGapMs` until the caller goes
 *  away, which is counted as an abandon, or a reset ends the stream. */
function endless(request: Request, owner: FakeChatState, record: ChatCallRecord): Response {
  let ended = false;
  let end = () => {};
  const leave = () => {
    if (ended) return;
    ended = true;
    owner.abandoned++;
    record.left = Date.now();
    endlessStreams.delete(end);
  };
  request.signal.addEventListener("abort", leave, { once: true });
  const body = new ReadableStream<Uint8Array>({
    async start(controller) {
      end = () => {
        ended = true;
        endlessStreams.delete(end);
        controller.error(new Error("the fake chat model was reset"));
      };
      endlessStreams.add(end);
      try {
        controller.enqueue(event(frame({ delta: { role: "assistant" } })));
        while (!ended && !request.signal.aborted) {
          await Bun.sleep(endlessGapMs);
          if (ended || request.signal.aborted) return;
          controller.enqueue(event(frame({ delta: { content: "and more " } })));
        }
      } catch {
        leave();
      }
    },
    cancel() {
      leave();
    },
  });
  return eventStream(body);
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

/** A tool call as the wire carries it: its name, and its arguments as JSON text. */
interface WireCall {
  readonly name: string;
  readonly text: string;
}

/** `calls` with their arguments written as the wire carries them, once. */
function onTheWire(calls: readonly ChatToolCall[]): WireCall[] {
  return calls.map((call) => ({ name: call.name, text: jsonText(call.arguments, "{}") }));
}

/** The frames of `calls`, each with an id of this listener's own, the first piece of its
 *  arguments beside its name and the rest in frames of their own. `served` is told each id. */
function toolCallFrames(
  calls: readonly WireCall[],
  size: number,
  served: (id: string) => void
): unknown[] {
  const frames: unknown[] = [];
  for (const [index, call] of calls.entries()) {
    state.ids++;
    const id = `call_e2e_${state.ids}`;
    served(id);
    const [first = "", ...rest] = pieces(call.text, size);
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
  return frames;
}

/** The turn `reply` writes: its tool calls, or its content, then its finish_reason. */
function turn(reply: ChatReply, structured: boolean, toolTurn: boolean): unknown[] {
  const size = reply.chunk !== undefined && reply.chunk > 0 ? reply.chunk : defaultChunk;
  const frames: unknown[] = [frame({ delta: { role: "assistant" } })];
  if (toolTurn) {
    frames.push(
      ...toolCallFrames(onTheWire(reply.toolCalls ?? []), size, (id) =>
        state.pending.set(id, reply)
      )
    );
    frames.push(frame({ finish_reason: "tool_calls" }));
    return frames;
  }
  for (const piece of pieces(content(reply, structured), size)) {
    frames.push(frame({ delta: { content: piece } }));
  }
  frames.push(frame({ finish_reason: "stop" }));
  return frames;
}

/** The frames of one turn of a run: its prose, its tool calls, the finish they make, and the
 *  usage chunk with no choice that closes a stream which counts its tokens. */
function runTurn(scripted: ChatTurn): unknown[] {
  const size = scripted.chunk !== undefined && scripted.chunk > 0 ? scripted.chunk : defaultChunk;
  const calls = onTheWire(scripted.toolCalls ?? []);
  const frames: unknown[] = [frame({ delta: { role: "assistant" } })];
  for (const piece of pieces(scripted.text ?? "", size)) {
    frames.push(frame({ delta: { content: piece } }));
  }
  frames.push(...toolCallFrames(calls, size, () => {}));
  frames.push(frame({ finish_reason: calls.length > 0 ? "tool_calls" : "stop" }));
  const written =
    (scripted.text ?? "").length + calls.reduce((sum, call) => sum + call.text.length, 0);
  const output = Math.max(1, Math.ceil(written / 4));
  frames.push({
    choices: [],
    created: 1,
    id: "chatcmpl-e2e-fake",
    model: "e2e-fake-chat",
    object: "chat.completion.chunk",
    usage: { completion_tokens: output, prompt_tokens: 10, total_tokens: 10 + output },
  });
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

/** Answers one call of the agent run `run` with its script's next turn. */
function answerRun(
  request: Request,
  owner: FakeChatState,
  record: ChatCallRecord,
  run: RunState
): Response {
  record.turn = run.taken;
  const scripted = run.script.turns[run.taken] ?? run.script.afterwards;
  run.taken++;
  if (scripted === undefined) {
    return refuse(
      500,
      `the fake chat model has no turn left for the run of ${JSON.stringify(run.script.words)}`
    );
  }
  if (scripted.status !== undefined) {
    return refuse(scripted.status, "the model service refused this call", scripted.retryAfter);
  }
  if (scripted.endless === true) return endless(request, owner, record);
  return stream(request, owner, record, runTurn(scripted), scripted.delayMs ?? 0);
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
  const { context, words } = runOf(body);
  const record: ChatCallRecord = {
    at: Date.now(),
    context,
    left: null,
    toolResults: (body.messages ?? [])
      .filter((message) => message.role === "tool")
      .map((message) => messageText(message.content)),
    tools: (body.tools ?? []).map((tool) => tool.function.name),
    turn: null,
    words,
  };
  owner.received.push(record);
  const run = words === null ? undefined : state.runs.get(words);
  if (run !== undefined) return answerRun(request, owner, record, run);
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
  return stream(request, owner, record, turn(reply, structured, toolTurn), reply.delayMs ?? 0);
}

/** The runs of a script, by their words, or the refusal of a script that names one run's words
 *  twice or a run with no words. */
function scriptedRuns(runs: readonly ChatRunScript[]): Map<string, RunState> | string {
  const byWords = new Map<string, RunState>();
  for (const run of runs) {
    if (typeof run.words !== "string" || run.words === "" || !Array.isArray(run.turns)) {
      return "each run names its words and its turns";
    }
    if (byWords.has(run.words)) {
      return `the run of ${JSON.stringify(run.words)} is scripted twice: give one run all its turns`;
    }
    byWords.set(run.words, { script: run, taken: 0 });
  }
  return byWords;
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
        received: state.received,
        requests: state.requests,
        structured: state.structured,
        unauthorized: state.unauthorized,
      };
      return Response.json(calls);
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/script") {
      const script = (await request.json()) as ChatScript;
      const runs = scriptedRuns(script.runs ?? []);
      if (typeof runs === "string") return Response.json({ error: runs }, { status: 400 });
      // In place, so a call already counting on this row's counts keeps counting on them.
      state.fallback = script.default;
      state.pending = new Map();
      state.queue = [...(script.replies ?? [])];
      state.runs = runs;
      return Response.json({ ok: true });
    }
    if (request.method === "PUT" && url.pathname === "/__fixture/reset") {
      for (const end of [...endlessStreams]) end();
      state = emptyState();
      return Response.json({ ok: true });
    }
    if (url.pathname === "/healthz") return Response.json({ ok: true });
    return Response.json({ message: "not found" }, { status: 404 });
  },
});

console.log(`fake chat model listener on 127.0.0.1:${server.port}`);
