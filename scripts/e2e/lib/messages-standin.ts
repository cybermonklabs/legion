// The model a proof's agents reach: a stand-in on loopback for the three wire protocols the proofs'
// providers speak, Anthropic Messages, OpenAI Chat Completions and OpenAI Responses, so a run takes
// no model and no key from outside the runner. Every harness calls it: Oh My Pi through its
// profile's provider, whose baseUrl names this server (or ANTHROPIC_BASE_URL, for its built-in
// anthropic provider), Claude Code through ANTHROPIC_BASE_URL, and Codex CLI through a model
// provider of its config.toml, whose base_url names this server's /v1 (wire_api responses).
//
//   bun scripts/e2e/lib/messages-standin.ts <port-file> <key-file> <requests-file> \
//     [<reply> [<model>...]]
//
// answers every turn with one text block, <reply> (default `ready`): stage2-tmux-supervision.sh,
// clean-session-stop-tmux.sh and nats-callout-sessions.sh run it so. Each <model> is one its model
// listing names: a Responses endpoint's preflight (lib/model-endpoint.sh's responses_key_preflight)
// asks the listing for the model Codex runs, which nats-callout-sessions.sh names. A proof whose
// agents must call tools, or answer what they were asked, runs a script of its own instead, a
// module that hands `runStandin` its reply and takes the same three arguments:
// lib/session-settings-model.ts, lib/user-turns-model.ts, lib/session-delivery-model.ts and
// lib/plugin-smoke-model.ts.
//
// It serves on a port the kernel chose and writes that port to <port-file> once it answers, so the
// caller reads it as it reads a held port (lib/port-file.sh). Every request must carry the
// placeholder key <key-file> holds, as `x-api-key` or as an `Authorization: Bearer`, which is how
// each harness sends the key its profile, its key command or its environment gave it; any other is
// answered 401, so a turn that reached the model proves the pane's key route did. It answers, under
// any path prefix:
//   GET  …/models                    a listing of the <model>s, empty where none is named, as the
//                                    daemon's own stand-ins answer;
//   POST …/v1/messages/count_tokens  a count;
//   POST …/v1/messages               an Anthropic Messages turn: streamed when the request
//                                    streams, in the events `messageStream` of
//                                    @legion/pi-shared/test/omp-harness writes for the plugins' own
//                                    Oh My Pi tests, and as one message when it does not;
//   POST …/chat/completions          an OpenAI Chat Completions turn: streamed as chunks when the
//                                    request streams, and as one completion when it does not;
//   POST …/responses                 an OpenAI Responses turn: streamed when the request streams,
//                                    in the events lib/responses-stream.ts writes (Codex streams
//                                    every turn), and as one response when it does not;
// and 404 for anything else. Each request is one JSON line in <requests-file>: its method, path,
// model, whether it carried the key, the end of its last user text, and what the stand-in
// answered, which a proof reads to say what the stand-in served. It runs until it is signalled.
import { appendFileSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import {
  type Block,
  messageStream,
  type Request,
} from "../../../packages/pi-shared/test/omp-harness.ts";
import {
  type ResponsesItem,
  responsesCall,
  responsesMessage,
  responsesStream,
} from "./responses-stream.ts";

/** One message of a turn's conversation, read the same from each protocol. */
export interface TurnMessage {
  readonly role: "user" | "assistant";
  /** Its text blocks, joined by blank lines. */
  readonly text: string;
  /** The tools an assistant message called, in order. */
  readonly calls: readonly { readonly name: string; readonly input: unknown }[];
  /** The text of each tool result a user message carries. OpenAI's `tool` messages and Responses'
   *  `function_call_output` items are folded into one user message, as Anthropic's request
   *  carries them. */
  readonly results: readonly string[];
}

/** What a reply reads of one request. */
export interface Turn {
  readonly api: "anthropic" | "openai" | "responses";
  readonly model: string;
  /** The system prompt's text. */
  readonly system: string;
  /** The conversation, oldest first; the system prompt is not among them. */
  readonly messages: readonly TurnMessage[];
  /** Each tool the request offers, by name, with the names of its input's properties. */
  readonly tools: ReadonlyMap<string, readonly string[]>;
  /** The request as sent, for omp-harness's helpers that read one (isSelfCheck, isSummarization). */
  readonly request: Request;
}

/** What a scripted stand-in answers a turn with, the blocks of one assistant message. */
export type TurnReply = (turn: Turn) => readonly Block[];

/** A reply of one text block. */
export function text(value: string): Block[] {
  return [{ type: "text", text: value }];
}

/** `value` as one word of a shell command. */
export function quoted(value: string): string {
  return `'${value.replaceAll("'", "'\\''")}'`;
}

/** The prompt the model is running on — the last user messages that carry text and no tool result,
 *  joined (a host may put context it injects in a message of its own beside the prompt) — and
 *  every tool the assistant called after it, in order. Undefined when there is none. */
export function lastPrompt(
  turn: Turn
): { readonly text: string; readonly calls: readonly string[] } | undefined {
  const prompt = (message: TurnMessage | undefined) =>
    message !== undefined &&
    message.role === "user" &&
    message.results.length === 0 &&
    message.text.trim() !== "";
  let end = turn.messages.length - 1;
  while (end >= 0 && !prompt(turn.messages[end])) end--;
  if (end < 0) return undefined;
  let start = end;
  while (start > 0 && prompt(turn.messages[start - 1])) start--;
  return {
    text: turn.messages
      .slice(start, end + 1)
      .map((message) => message.text)
      .join("\n\n"),
    calls: turn.messages.slice(end + 1).flatMap((later) => later.calls.map((call) => call.name)),
  };
}

/** Whether the request answers a tool call: its last message carries a tool result. */
export function afterToolResult(turn: Turn): boolean {
  return (turn.messages.at(-1)?.results.length ?? 0) > 0;
}

/** A call of tool `name` with `input`, or undefined when the request offers no such tool. An Oh My
 *  Pi tool requires the one-line intent `i` (omp-harness's `call`), which is added when the
 *  offered tool's input names it, and only then: a tool of Claude Code's takes no such field. */
export function toolCall(
  turn: Turn,
  name: string,
  input: Record<string, unknown>
): Block | undefined {
  const properties = turn.tools.get(name);
  if (properties === undefined) return undefined;
  return {
    type: "tool_use",
    name,
    input: properties.includes("i") ? { i: "Running the proof's step", ...input } : input,
  };
}

// ---- reading a request ------------------------------------------------------------------------

function record(value: unknown): Record<string, unknown> {
  return typeof value === "object" && value !== null ? (value as Record<string, unknown>) : {};
}

/** The text of a content value: a string, or the text of its `text` blocks joined. */
function contentText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((block) => {
      const each = record(block);
      return each.type === "text" && typeof each.text === "string" ? each.text : "";
    })
    .filter((each) => each !== "")
    .join("\n\n");
}

function anthropicTurn(body: Record<string, unknown>, request: Request): Turn {
  const messages = (Array.isArray(body.messages) ? body.messages : []).map((raw): TurnMessage => {
    const message = record(raw);
    const blocks = Array.isArray(message.content) ? message.content.map(record) : [];
    return {
      role: message.role === "assistant" ? "assistant" : "user",
      text: contentText(message.content),
      calls: blocks
        .filter((block) => block.type === "tool_use")
        .map((block) => ({ name: String(block.name), input: block.input })),
      results: blocks
        .filter((block) => block.type === "tool_result")
        .map((block) => contentText(block.content)),
    };
  });
  const tools = new Map(
    (Array.isArray(body.tools) ? body.tools.map(record) : []).map((tool) => [
      String(tool.name),
      Object.keys(record(record(tool.input_schema).properties)),
    ])
  );
  return {
    api: "anthropic",
    model: typeof body.model === "string" ? body.model : "",
    system: contentText(body.system),
    messages,
    tools,
    request,
  };
}

function openaiTurn(body: Record<string, unknown>, request: Request): Turn {
  const system: string[] = [];
  const messages: TurnMessage[] = [];
  for (const raw of Array.isArray(body.messages) ? body.messages : []) {
    const message = record(raw);
    if (message.role === "system" || message.role === "developer") {
      system.push(contentText(message.content));
    } else if (message.role === "tool") {
      const last = messages.at(-1);
      const result = contentText(message.content);
      if (
        last !== undefined &&
        last.role === "user" &&
        last.text === "" &&
        last.results.length > 0
      ) {
        messages[messages.length - 1] = { ...last, results: [...last.results, result] };
      } else {
        messages.push({ role: "user", text: "", calls: [], results: [result] });
      }
    } else {
      const calls = (Array.isArray(message.tool_calls) ? message.tool_calls.map(record) : []).map(
        (call) => {
          const fn = record(call.function);
          let input: unknown = {};
          try {
            input = JSON.parse(String(fn.arguments ?? "{}"));
          } catch {
            input = fn.arguments;
          }
          return { name: String(fn.name), input };
        }
      );
      messages.push({
        role: message.role === "assistant" ? "assistant" : "user",
        text: contentText(message.content),
        calls,
        results: [],
      });
    }
  }
  const tools = new Map(
    (Array.isArray(body.tools) ? body.tools.map(record) : []).map((tool) => {
      const fn = record(tool.function);
      return [String(fn.name), Object.keys(record(record(fn.parameters).properties))] as const;
    })
  );
  return {
    api: "openai",
    model: typeof body.model === "string" ? body.model : "",
    system: system.join("\n\n"),
    messages,
    tools,
    request,
  };
}

/** The text of a Responses content value: a string, or the text of its text parts joined. */
function responsesText(content: unknown): string {
  if (typeof content === "string") return content;
  if (!Array.isArray(content)) return "";
  return content
    .map((part) => {
      const each = record(part);
      return ["input_text", "output_text", "text"].includes(String(each.type)) &&
        typeof each.text === "string"
        ? each.text
        : "";
    })
    .filter((each) => each !== "")
    .join("\n\n");
}

/** A Responses request read as a turn: its `input` items in order, a function call joining the
 *  assistant message before it and an output the user message of results before it, as
 *  Anthropic's request carries them; its `instructions` and system or developer messages are the
 *  system prompt, and an item a reply reads nothing of (reasoning, say) is passed over. */
function responsesTurn(body: Record<string, unknown>, request: Request): Turn {
  const system: string[] = [];
  if (typeof body.instructions === "string" && body.instructions !== "") {
    system.push(body.instructions);
  }
  const messages: TurnMessage[] = [];
  const input =
    typeof body.input === "string"
      ? [{ type: "message", role: "user", content: body.input }]
      : Array.isArray(body.input)
        ? body.input
        : [];
  for (const raw of input) {
    const item = record(raw);
    const last = messages.at(-1);
    if (item.type === "function_call") {
      let parsed: unknown = {};
      try {
        parsed = JSON.parse(String(item.arguments ?? "{}"));
      } catch {
        parsed = item.arguments;
      }
      const call = { name: String(item.name), input: parsed };
      if (last !== undefined && last.role === "assistant") {
        messages[messages.length - 1] = { ...last, calls: [...last.calls, call] };
      } else {
        messages.push({ role: "assistant", text: "", calls: [call], results: [] });
      }
    } else if (item.type === "function_call_output") {
      const result = responsesText(item.output);
      if (
        last !== undefined &&
        last.role === "user" &&
        last.text === "" &&
        last.results.length > 0
      ) {
        messages[messages.length - 1] = { ...last, results: [...last.results, result] };
      } else {
        messages.push({ role: "user", text: "", calls: [], results: [result] });
      }
    } else if (item.type === undefined || item.type === "message") {
      if (item.role === "system" || item.role === "developer") {
        system.push(responsesText(item.content));
      } else {
        messages.push({
          role: item.role === "assistant" ? "assistant" : "user",
          text: responsesText(item.content),
          calls: [],
          results: [],
        });
      }
    }
  }
  const tools = new Map(
    (Array.isArray(body.tools) ? body.tools.map(record) : [])
      .filter((tool) => tool.type === "function")
      .map((tool) => [String(tool.name), Object.keys(record(record(tool.parameters).properties))])
  );
  return {
    api: "responses",
    model: typeof body.model === "string" ? body.model : "",
    system: system.join("\n\n"),
    messages,
    tools,
    request,
  };
}

// ---- answering --------------------------------------------------------------------------------

function anthropicMessage(blocks: readonly Block[], id: string, model: string): Response {
  const toolUse = blocks.some((block) => block.type === "tool_use");
  return Response.json({
    id,
    type: "message",
    role: "assistant",
    model,
    content: blocks.map((block, index) =>
      block.type === "text"
        ? block
        : { type: "tool_use", id: `toolu_${id}_${index}`, name: block.name, input: block.input }
    ),
    stop_reason: toolUse ? "tool_use" : "end_turn",
    stop_sequence: null,
    usage: { input_tokens: 5, output_tokens: 1 },
  });
}

/** The tool calls of `blocks` as Chat Completions writes them, each with its index among them. */
function openaiCalls(blocks: readonly Block[], id: string) {
  return blocks.flatMap((block, index) =>
    block.type === "tool_use"
      ? [
          {
            id: `call_${id}_${index}`,
            type: "function",
            function: { name: block.name, arguments: JSON.stringify(block.input) },
          },
        ]
      : []
  );
}

function openaiStream(blocks: readonly Block[], id: string, model: string): string {
  const chunk = (delta: Record<string, unknown>, finish: string | null) => ({
    id,
    object: "chat.completion.chunk",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [{ index: 0, delta, finish_reason: finish }],
  });
  const content = blocks.flatMap((block) => (block.type === "text" ? [block.text] : [])).join("");
  const calls = openaiCalls(blocks, id);
  const chunks: unknown[] = [chunk({ role: "assistant", content: "" }, null)];
  if (content !== "") chunks.push(chunk({ content }, null));
  calls.forEach((call, index) => {
    chunks.push(chunk({ tool_calls: [{ index, ...call }] }, null));
  });
  chunks.push(chunk({}, calls.length > 0 ? "tool_calls" : "stop"));
  chunks.push({
    id,
    object: "chat.completion.chunk",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [],
    usage: { prompt_tokens: 5, completion_tokens: 1, total_tokens: 6 },
  });
  return `${chunks.map((each) => `data: ${JSON.stringify(each)}\n\n`).join("")}data: [DONE]\n\n`;
}

function openaiCompletion(blocks: readonly Block[], id: string, model: string): Response {
  const content = blocks.flatMap((block) => (block.type === "text" ? [block.text] : [])).join("");
  const calls = openaiCalls(blocks, id);
  return Response.json({
    id,
    object: "chat.completion",
    created: Math.floor(Date.now() / 1000),
    model,
    choices: [
      {
        index: 0,
        message: {
          role: "assistant",
          content: content === "" ? null : content,
          ...(calls.length > 0 ? { tool_calls: calls } : {}),
        },
        finish_reason: calls.length > 0 ? "tool_calls" : "stop",
      },
    ],
    usage: { prompt_tokens: 5, completion_tokens: 1, total_tokens: 6 },
  });
}

/** `blocks` as Responses output items: their text as one assistant message, then each call. */
function responsesItems(blocks: readonly Block[], id: string): ResponsesItem[] {
  const content = blocks.flatMap((block) => (block.type === "text" ? [block.text] : [])).join("");
  const items = content === "" ? [] : [responsesMessage(`${id}_message`, content)];
  blocks.forEach((block, index) => {
    if (block.type === "tool_use") {
      items.push(
        responsesCall(
          `fc_${id}_${index}`,
          `call_${id}_${index}`,
          block.name,
          JSON.stringify(block.input)
        )
      );
    }
  });
  return items;
}

function responsesResponse(items: readonly ResponsesItem[], id: string, model: string): Response {
  return Response.json({
    id,
    object: "response",
    status: "completed",
    model,
    output: items,
    usage: { input_tokens: 5, output_tokens: 1, total_tokens: 6 },
  });
}

function refusal(status: number, type: string, message: string): Response {
  return Response.json({ type: "error", error: { type, message } }, { status });
}

/** Serves `reply` as the model, on the three arguments the header names, its model listing naming
 *  `listed`; never returns. */
export function runStandin(reply: TurnReply, listed: readonly string[] = []): void {
  const [portFile, keyFile, requestsFile] = process.argv.slice(2);
  if (portFile === undefined || keyFile === undefined || requestsFile === undefined) {
    process.stderr.write(
      `usage: bun ${process.argv[1] ?? "messages-standin.ts"} <port-file> <key-file> <requests-file>${import.meta.main ? " [<reply> [<model>...]]" : ""}\n`
    );
    process.exit(2);
  }
  const key = readFileSync(keyFile, "utf8").trim();
  if (key === "") {
    process.stderr.write(`messages-standin: ${keyFile} holds no key\n`);
    process.exit(2);
  }

  let answered = 0;
  const server = Bun.serve({
    port: 0,
    hostname: "127.0.0.1",
    async fetch(request) {
      const url = new URL(request.url);
      const raw = request.method === "POST" ? await request.text() : "";
      const body = raw === "" ? {} : record(JSON.parse(raw));
      // The placeholder key, in either header a harness sends it in.
      const carried =
        request.headers.get("x-api-key") === key ||
        request.headers.get("authorization") === `Bearer ${key}`;
      const chat = url.pathname.endsWith("/chat/completions");
      const responses = url.pathname.endsWith("/responses");
      const turn = chat
        ? openaiTurn(body, { path: url.pathname, body })
        : responses
          ? responsesTurn(body, { path: url.pathname, body })
          : anthropicTurn(body, { path: url.pathname, body });
      const lastUser = turn.messages.findLast((message) => message.role === "user");
      const log = (answer: unknown) =>
        appendFileSync(
          requestsFile,
          `${JSON.stringify({
            at: new Date().toISOString(),
            method: request.method,
            path: url.pathname,
            model: turn.model,
            keyed: carried,
            text: (lastUser?.text ?? "").slice(-4000),
            answer,
          })}\n`
        );
      if (!carried) {
        log("401");
        return chat || responses
          ? Response.json(
              { error: { message: "invalid api key", type: "invalid_request_error" } },
              { status: 401 }
            )
          : refusal(401, "authentication_error", "invalid x-api-key");
      }
      if (request.method === "GET" && url.pathname.endsWith("/models")) {
        log("models");
        return Response.json({
          object: "list",
          data: listed.map((id) => ({
            id,
            object: "model",
            type: "model",
            display_name: id,
            owned_by: "standin",
          })),
          has_more: false,
          first_id: listed[0] ?? null,
          last_id: listed.at(-1) ?? null,
        });
      }
      if (request.method === "POST" && url.pathname.endsWith("/v1/messages/count_tokens")) {
        log("count");
        return Response.json({ input_tokens: 5 });
      }
      if (
        request.method !== "POST" ||
        !(chat || responses || url.pathname.endsWith("/v1/messages"))
      ) {
        log("404");
        return refusal(404, "not_found_error", `no route ${url.pathname}`);
      }
      answered += 1;
      const id = `msg_standin_${answered}`;
      const blocks = reply(turn);
      log(blocks);
      if (responses) {
        const items = responsesItems(blocks, id);
        return body.stream === true
          ? new Response(responsesStream(id, items), {
              headers: { "content-type": "text/event-stream" },
            })
          : responsesResponse(items, id, turn.model);
      }
      if (chat) {
        return body.stream === true
          ? new Response(openaiStream(blocks, id, turn.model), {
              headers: { "content-type": "text/event-stream" },
            })
          : openaiCompletion(blocks, id, turn.model);
      }
      return body.stream === true
        ? new Response(messageStream(blocks, id), {
            headers: { "content-type": "text/event-stream" },
          })
        : anthropicMessage(blocks, id, turn.model);
    },
  });
  // The port is written whole, by a rename, so a reader never takes half of it.
  writeFileSync(`${portFile}.tmp`, `${server.port}\n`);
  renameSync(`${portFile}.tmp`, portFile);

  for (const signal of ["SIGTERM", "SIGINT", "SIGHUP"] as const) {
    process.on(signal, () => {
      void server.stop(true).finally(() => process.exit(0));
    });
  }
}

if (import.meta.main) {
  const reply = process.argv[5] ?? "ready";
  runStandin(() => text(reply), process.argv.slice(6));
}
