/** `bun outside-agent-codex-model.ts <plan.json> <log.jsonl>`: the keyless stand-in for the outside
 *  person's model that `outside-agent-codex.sh` runs Codex against. It speaks OpenAI's Responses
 *  API as Codex calls it (`wire_api = "responses"`): a POST to `/v1/responses`, answered with
 *  server-sent events, as `packages/claude-envoy/tests/codex-real.test.ts` answers. It binds a
 *  port of the kernel's choosing on 127.0.0.1, prints `{"port": N}`, answers `GET /_ready` with the
 *  plan's nonce, so the leg points Codex only at its own child, and serves until it is stopped.
 *
 *  It plays one scripted turn for each `codex exec` the leg runs, named by the prompt
 *  `outside-e2e codex-run <run> <nonce>`:
 *    main          finds the plugin's tools, calls `dispatch whoami` (C2), comments on the scratch
 *                  issue with `dispatch comment` (C3), runs `sleep` until a request carries the
 *                  message the probe sent the session (C4), then sends the probe's session a
 *                  message with `envoy_send` (C5), and ends the turn.
 *    edge-refused  finds the tools and calls `dispatch whoami` (C6), and ends the turn.
 *    inside        the same, for C2's control.
 *  Codex sends the whole turn so far in each request, so the next step is read from the request
 *  itself: which of this model's calls already have an output.
 *
 *  Codex lists the plugin's tools either in the request's `tools` (a `namespace` tool named
 *  `mcp__envoy` holding `dispatch`, `envoy_send` and the rest) or, for a model whose tools Codex
 *  defers (0.171.0 with `-m gpt-5.5`, measured), behind a `tool_search` tool: then the model first
 *  calls `tool_search_call` and finds them in the next request's `tool_search_output`.
 *
 *  It appends one line of compact JSON to the log for each thing the leg judges, every value a
 *  plain word (no quote, backslash or control character), so the leg reads it with `json_field`:
 *    {"run","step":"tools","dispatch","send"}  the tool names it found, or "" for none
 *    {"run","step":"whoami","exit","session","owner","project","elsewhere"}
 *    {"run","step":"comment","exit","comment_id"}
 *    {"run","step":"inbound","at","waits"}     the first request carrying the probe's message
 *    {"run","step":"control","at"}             a request carrying the control message
 *    {"run","step":"wait-ended","waits"}       the sleeps ran out with no message
 *    {"run","step":"send","at"}                the moment it called envoy_send
 *    {"run","step":"sent","ok","detail"}       that call's output
 *    {"run","step":"end"}
 *  `at` is this machine's clock in Unix ms. It holds no credential and prints no secret. */

import { appendFileSync, readFileSync } from "node:fs";

/** What the leg tells the model, in `plan.json`. */
export interface Plan {
  readonly nonce: string;
  /** The scratch issue C3 comments on, or "" when the leg found none. */
  readonly issue: string;
  readonly comment: string;
  readonly inbound: string;
  readonly control: string;
  readonly outbound: string;
  /** The session `envoy_send` sends `outbound` to: the probe's receiver. */
  readonly outboundTo: string;
  readonly waitCalls: number;
  readonly waitSeconds: number;
}

type Item = Record<string, unknown>;

/** A tool the model calls: its name, and the namespace Codex listed it in, if any. */
export interface Tool {
  readonly name: string;
  readonly namespace?: string;
}

/** A log line, before the run's name is added. */
export type LogLine = Record<string, string | number | boolean>;

/** What one request is answered with, and the lines it adds to the log. */
export interface Turn {
  readonly items: readonly Item[];
  readonly log: readonly LogLine[];
}

const text = (value: unknown): string => (typeof value === "string" ? value : "");

/** `value` as a plain word for the log: printable ASCII alone, with no quote or backslash, at most
 *  200 characters. */
export function word(value: string): string {
  return value.replace(/[^ -~]+|["\\]+/g, " ").slice(0, 200);
}

/** The plugin's tool `name`, as Codex lists it: a function of a namespace whose name holds
 *  `envoy`, or a function named `mcp__envoy__<name>`. */
export function findTool(tools: unknown, name: string): Tool | undefined {
  if (!Array.isArray(tools)) return undefined;
  for (const tool of tools as Item[]) {
    const own = text(tool.name);
    if (own === `mcp__envoy__${name}`) return { name: own };
    if (Array.isArray(tool.tools) && own.includes("envoy")) {
      for (const inner of tool.tools as Item[]) {
        if (text(inner.name) === name) return { name, namespace: own };
      }
    }
  }
  return undefined;
}

/** The run a request belongs to, from the first user message's `outside-e2e codex-run <run>`. */
export function runOf(input: readonly Item[]): string {
  for (const item of input) {
    if (item.role !== "user") continue;
    const run = /outside-e2e codex-run ([a-z-]+) /.exec(JSON.stringify(item.content ?? ""));
    if (run !== null) return run[1] as string;
  }
  return "";
}

/** The text of a tool call's output: a string, or the `text` of each part of a list. */
function outputText(output: unknown): string {
  if (typeof output === "string") return output;
  if (Array.isArray(output)) return output.map((part) => text((part as Item).text)).join("\n");
  return "";
}

/** The `dispatch` command's exit code, from the `[exit N]` line the tool ends its output with;
 *  -1 for an output with none (the call itself failed). */
function exitOf(output: string): number {
  const exit = /\[exit (\d+)\]\s*$/.exec(output);
  return exit === null ? -1 : Number(exit[1]);
}

/** `dispatch whoami`'s answer, as its text names it: "Session <s> acts for <owner>: ...", "...
 *  It may write project <key> and no other.", and "Your credential names session <x>, which is
 *  not this host session" when the credential proved another session; and, for a call that did
 *  not exit 0, its first words, such as the refusal of a configuration it cannot use. */
export function whoamiOf(output: string): LogLine {
  const exit = exitOf(output);
  return {
    exit,
    session: word(/Session (\S+) /.exec(output)?.[1] ?? ""),
    owner: word(/ acts for ([^\s:]+):/.exec(output)?.[1] ?? ""),
    project: word(/may write project (\S+) and no other/.exec(output)?.[1] ?? ""),
    elsewhere: word(/Your credential names session (\S+),/.exec(output)?.[1] ?? ""),
    ...(exit === 0 ? {} : { detail: word(output.replace(/\s+/g, " ").trim()) }),
  };
}

/** `dispatch comment`'s answer: its exit code and the comment id it names, if any. */
export function commentOf(output: string): LogLine {
  const id = /[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}/.exec(output);
  return { exit: exitOf(output), comment_id: id?.[0] ?? "" };
}

/** An `envoy_send` call's answer: ok unless Codex or the server answered an error. */
export function sentOf(output: string): LogLine {
  const failed = /tool call error|Mcp error|not ready|refused|failed/i.test(output);
  return { ok: !failed, detail: word(output.replace(/\s+/g, " ").trim()) };
}

function message(content: string, n: number): Item {
  return {
    content: [{ annotations: [], text: content, type: "output_text" }],
    id: `msg_${n}`,
    role: "assistant",
    status: "completed",
    type: "message",
  };
}

function call(tool: Tool, step: string, n: number, args: unknown): Item {
  return {
    arguments: JSON.stringify(args),
    call_id: `outside-${step}-${n}`,
    id: `fc_${n}`,
    name: tool.name,
    type: "function_call",
    ...(tool.namespace === undefined ? {} : { namespace: tool.namespace }),
  };
}

/** The next items of the turn a request belongs to, and what to log: a pure reading of the
 *  request, so a request Codex sends again is answered the same. `n` numbers the call ids;
 *  `logged` holds the call ids and marks already logged, which this adds to; `now` is the clock. */
export function nextTurn(
  plan: Plan,
  body: { readonly tools?: unknown; readonly input?: readonly Item[] },
  n: number,
  logged: Set<string>,
  now: number
): Turn {
  const input = body.input ?? [];
  const run = runOf(input);
  const log: LogLine[] = [];
  const once = (mark: string, line: LogLine): void => {
    if (logged.has(`${run}:${mark}`)) return;
    logged.add(`${run}:${mark}`);
    log.push(line);
  };
  if (run === "") return { items: [message("outside-e2e: no run named", n)], log };

  // The tools: in the request, else in a tool_search_output this turn already holds.
  let dispatch = findTool(body.tools, "dispatch");
  let send = findTool(body.tools, "envoy_send");
  let searched = false;
  for (const item of input) {
    if (item.type !== "tool_search_output") continue;
    searched = true;
    dispatch ??= findTool(item.tools, "dispatch");
    send ??= findTool(item.tools, "envoy_send");
  }
  const offersSearch =
    Array.isArray(body.tools) && (body.tools as Item[]).some((tool) => tool.type === "tool_search");
  if (dispatch === undefined && !searched && offersSearch) {
    return {
      items: [
        {
          arguments: { limit: 20, query: "envoy dispatch send" },
          call_id: `outside-search-${n}`,
          execution: "client",
          id: `ts_${n}`,
          status: "completed",
          type: "tool_search_call",
        },
      ],
      log,
    };
  }
  const named = (tool: Tool | undefined): string =>
    tool === undefined
      ? ""
      : word(`${tool.namespace ?? ""}${tool.namespace ? "." : ""}${tool.name}`);
  once("tools", { step: "tools", dispatch: named(dispatch), send: named(send) });
  if (dispatch === undefined || (run === "main" && send === undefined)) {
    once("end", { step: "end" });
    return { items: [message("outside-e2e: the plugin's tools are not listed", n)], log };
  }

  // The outputs of this model's calls so far, by step, in order.
  const outputs = new Map<string, string>();
  for (const item of input) {
    if (item.type === "function_call_output")
      outputs.set(text(item.call_id), outputText(item.output));
  }
  const calls = input.filter(
    (item) => item.type === "function_call" && text(item.call_id).startsWith("outside-")
  );
  const answered = (step: string): string | undefined => {
    for (const each of calls) {
      const id = text(each.call_id);
      if (id.startsWith(`outside-${step}-`) && outputs.has(id)) return outputs.get(id);
    }
    return undefined;
  };
  const whole = JSON.stringify(input);
  if (whole.includes(plan.control)) once("control", { step: "control", at: now });

  const whoami = answered("whoami");
  if (whoami === undefined)
    return { items: [call(dispatch, "whoami", n, { args: ["whoami"] })], log };
  once("whoami", { step: "whoami", ...whoamiOf(whoami) });
  if (run !== "main") {
    once("end", { step: "end" });
    return { items: [message(`outside-e2e ${run} done`, n)], log };
  }

  if (plan.issue !== "") {
    const comment = answered("comment");
    if (comment === undefined) {
      return {
        items: [
          call(dispatch, "comment", n, {
            args: ["comment", "--issue", plan.issue, "--body-file", "-"],
            stdin: plan.comment,
          }),
        ],
        log,
      };
    }
    once("comment", { step: "comment", ...commentOf(comment) });
  }

  const waits = calls.filter((each) => text(each.call_id).startsWith("outside-wait-")).length;
  if (!whole.includes(plan.inbound)) {
    if (waits < plan.waitCalls) {
      const shell: Tool = { name: "exec_command" };
      return { items: [call(shell, "wait", n, { cmd: `sleep ${plan.waitSeconds}` })], log };
    }
    once("wait-ended", { step: "wait-ended", waits });
    once("end", { step: "end" });
    return { items: [message("outside-e2e: no message reached the session", n)], log };
  }
  once("inbound", { step: "inbound", at: now, waits });

  const sent = answered("send");
  if (sent === undefined) {
    once("send", { step: "send", at: now });
    return {
      items: [
        call(send as Tool, "send", n, { message: plan.outbound, session_id: plan.outboundTo }),
      ],
      log,
    };
  }
  once("sent", { step: "sent", ...sentOf(sent) });
  once("end", { step: "end" });
  return { items: [message("outside-e2e main done", n)], log };
}

/** The server-sent events of one response holding `items`. */
export function events(id: string, items: readonly Item[]): string {
  const all: Item[] = [{ response: { id }, type: "response.created" }];
  for (const item of items) all.push({ item, type: "response.output_item.done" });
  all.push({
    response: {
      id,
      usage: {
        input_tokens: 1,
        input_tokens_details: { cached_tokens: 0 },
        output_tokens: 1,
        output_tokens_details: { reasoning_tokens: 0 },
        total_tokens: 2,
      },
    },
    type: "response.completed",
  });
  return all.map((event) => `event: ${event.type}\ndata: ${JSON.stringify(event)}\n\n`).join("");
}

if (import.meta.main) {
  const [planFile = "", logFile = ""] = Bun.argv.slice(2);
  if (planFile === "" || logFile === "") {
    process.stdout.write(`${JSON.stringify({ unknown: "usage: <plan.json> <log.jsonl>" })}\n`);
    process.exit(2);
  }
  const plan = JSON.parse(readFileSync(planFile, "utf8")) as Plan;
  const logged = new Set<string>();
  let requests = 0;
  const server = Bun.serve({
    hostname: "127.0.0.1",
    port: 0,
    async fetch(request) {
      const url = new URL(request.url);
      if (request.method === "GET" && url.pathname === "/_ready") return new Response(plan.nonce);
      if (request.method !== "POST" || !url.pathname.endsWith("/responses")) {
        return Response.json({ error: { message: `no route ${url.pathname}` } }, { status: 404 });
      }
      requests += 1;
      let body: { tools?: unknown; input?: Item[] } = {};
      try {
        body = (await request.json()) as typeof body;
      } catch {
        // A body that is no JSON is a request with no input, answered with a message.
      }
      const turn = nextTurn(plan, body, requests, logged, Date.now());
      const run = runOf(body.input ?? []);
      for (const line of turn.log) appendFileSync(logFile, `${JSON.stringify({ run, ...line })}\n`);
      return new Response(events(`resp_${requests}`, turn.items), {
        headers: { "content-type": "text/event-stream" },
      });
    },
  });
  process.stdout.write(`${JSON.stringify({ port: server.port })}\n`);
}
