#!/usr/bin/env bun
/**
 * The model stand-in of a scored run (rig.sh, SKILL_SCENARIOS_MODEL=standin): the model gateway
 * for one run's agents, which answers every turn with one fixed assistant message and calls no
 * tool. It is the test harness's stand-in server (`serveStandin`) and its Anthropic Messages
 * stream (`messageStream`) of `@legion/pi-shared/test/omp-harness`, so a run on it is the same
 * real Oh My Pi, plugins, Dispatch, listener and daemon stand-in as a run on a real model, with
 * the model alone stood in. A run on it scores and fails: the model writes no plan and reads
 * nothing, so each clause a program decides fails by name, and each judged clause is pending
 * until its judgment.
 *
 * The profile's provider block names it in place of the gateway (rig.sh install_standin_route,
 * with rig.sh's model_standin): the provider `standin` and its one model `standin-model`. The
 * stand-in answers whatever model a request names, and the transcript names
 * `standin/standin-model`, the `model` line of the label's built-from.
 *
 * Its second mode, SKILL_SCENARIOS_MODEL=standin-report, is a path check's scripted model (C4 of
 * the forward-only plan). It plays the two calls of `standin-script.json` beside its log
 * (STANDIN_SCRIPT, which rig.sh's forward_script writes), `{calls: [{tool, input}, {tool, input}]}`,
 * by the tool results a Messages request carries back of the calls it answered: with none it
 * answers one `tool_use` block of the first call, with one the second call, and with two its fixed
 * message. A request that carries one logs that result's text as `toolResult`. A first turn before
 * the script is written waits for it, up to SCRIPT_WAIT_MS.
 *
 * Usage: `bun model-standin.ts <log file>`. Every request appends one JSON line to the log,
 * `{at, method, path, answered}`, `answered` the number of the Messages turn it answered or null,
 * and in the mode standin-report `toolResult` too, the text of the newest tool result of a scripted
 * call that the request carries, or null. `POST /anthropic/v1/messages` answers as an event stream;
 * any other path under `/anthropic/` answers `{"data":[]}`, as the harness's runner does; anything
 * else is a 404 naming the path. It prints `listening on <base URL>` once it serves, the base a
 * provider block's `baseUrl` is `<base URL>/anthropic` of. It serves until it is signalled.
 */
import { appendFileSync, existsSync, readFileSync } from "node:fs";
import path from "node:path";
import {
  type Block,
  type Cleanup,
  call,
  messageStream,
  serveStandin,
  toolResultsIn,
} from "@legion/pi-shared/test/omp-harness";
import { z } from "zod";

/** The one message the stand-in answers every turn with. */
export const MODEL_STANDIN_MESSAGE =
  "This is the model stand-in of the skill-scenario rig: it answers every turn with this one message and calls no tool.";
/** The script of the mode standin-report, beside the log. */
export const STANDIN_SCRIPT = "standin-script.json";
/** `standin-script.json`: the two calls a path check's model makes, in order. */
export const StandinScript = z.strictObject({
  calls: z
    .array(z.strictObject({ tool: z.string().min(1), input: z.record(z.string(), z.unknown()) }))
    .length(2),
});
/** How long a turn waits for the script before it answers with an error naming the file. */
const SCRIPT_WAIT_MS = 30_000;
/** The Messages route of the provider block's `baseUrl` (`<base>/anthropic`). */
const MESSAGES = "/anthropic/v1/messages";
/** The prefix of every tool_use id the stand-in answers (messageStream's `toolu_<id>_<index>`). */
const OWN_CALL = "toolu_msg_standin_";

/** The script `file` holds, once it is written and parses, or a reason it never did. */
async function readScript(file: string): Promise<z.infer<typeof StandinScript> | string> {
  const deadline = Date.now() + SCRIPT_WAIT_MS;
  let why = `${file} was never written`;
  while (Date.now() < deadline) {
    if (existsSync(file)) {
      try {
        return StandinScript.parse(JSON.parse(readFileSync(file, "utf8")));
      } catch (error) {
        why = `${file} is no script: ${error instanceof Error ? error.message : String(error)}`;
      }
    }
    await Bun.sleep(100);
  }
  return why;
}

if (import.meta.main) {
  const [log, ...rest] = Bun.argv.slice(2);
  if (log === undefined || rest.length > 0) {
    console.error("usage: bun model-standin.ts <log file>");
    process.exit(2);
  }
  const report = process.env.SKILL_SCENARIOS_MODEL === "standin-report";
  const scriptFile = path.join(path.dirname(log), STANDIN_SCRIPT);
  const cleanup: Cleanup = [];
  let turns = 0;
  const { base } = serveStandin(cleanup, async (url, body, request) => {
    const messages = request.method === "POST" && url.pathname === MESSAGES;
    if (messages) turns += 1;
    const turn = turns;
    const line: Record<string, unknown> = {
      at: new Date().toISOString(),
      method: request.method,
      path: url.pathname,
      answered: messages ? turn : null,
    };
    let blocks: Block[] = [{ type: "text", text: MODEL_STANDIN_MESSAGE }];
    let failure: string | undefined;
    if (messages && report) {
      const results = toolResultsIn({ path: url.pathname, body }).filter((result) =>
        result.tool_use_id.startsWith(OWN_CALL)
      );
      line.toolResult = results.at(-1)?.text ?? null;
      const script = await readScript(scriptFile);
      if (typeof script === "string") failure = script;
      else {
        const next = script.calls[results.length];
        if (next !== undefined) blocks = [call(next.tool, next.input)];
      }
    } else if (report) line.toolResult = null;
    appendFileSync(log, `${JSON.stringify(line)}\n`);
    if (failure !== undefined) return Response.json({ error: failure }, { status: 500 });
    if (messages) {
      return new Response(messageStream(blocks, `msg_standin_${turn}`), {
        headers: { "content-type": "text/event-stream" },
      });
    }
    if (url.pathname.startsWith("/anthropic/")) return Response.json({ data: [] });
    return Response.json({ error: `no stand-in route ${url.pathname}` }, { status: 404 });
  });
  const stop = async () => {
    for (const step of cleanup.reverse()) await step();
    process.exit(0);
  };
  process.on("SIGTERM", stop);
  process.on("SIGINT", stop);
  console.log(`listening on ${base}`);
}
