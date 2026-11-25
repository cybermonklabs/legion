#!/usr/bin/env bun
/**
 * The model stand-in of a scored run (rig.sh, SKILL_SCENARIOS_MODEL=standin): the model gateway
 * for one run's agents, which answers every turn with one fixed assistant message and calls no
 * tool. It is the test harness's stand-in server (`serveStandin`) and its Anthropic Messages
 * stream (`messageStream`) of `@legion/pi-shared/test/omp-harness`, so a run on it is the same
 * real Oh My Pi, plugins, Dispatch, listener and daemon stand-in as a run on a real model, with
 * the model alone stood in. A run on it scores, and fails each clause by name: the model writes
 * no plan and reads nothing.
 *
 * The profile's provider block names it in place of the gateway (rig.sh install_standin_route,
 * with rig.sh's model_standin): the provider `standin` and its one model `standin-model`. The
 * stand-in answers whatever model a request names, and the transcript names
 * `standin/standin-model`, the `model` line of the label's built-from.
 *
 * Usage: `bun model-standin.ts <log file>`. Every request appends one JSON line to the log,
 * `{at, method, path, answered}`, `answered` the number of the Messages turn it answered or null.
 * `POST /anthropic/v1/messages` answers the fixed message as an event stream; any other path
 * under `/anthropic/` answers `{"data":[]}`, as the harness's runner does; anything else is a 404
 * naming the path. It prints `listening on <base URL>` once it serves, the base a provider block's
 * `baseUrl` is `<base URL>/anthropic` of. It serves until it is signalled.
 */
import { appendFileSync } from "node:fs";
import { type Cleanup, messageStream, serveStandin } from "@legion/pi-shared/test/omp-harness";

/** The one message the stand-in answers every turn with. */
export const MODEL_STANDIN_MESSAGE =
  "This is the model stand-in of the skill-scenario rig: it answers every turn with this one message and calls no tool.";
/** The Messages route of the provider block's `baseUrl` (`<base>/anthropic`). */
const MESSAGES = "/anthropic/v1/messages";

if (import.meta.main) {
  const [log, ...rest] = Bun.argv.slice(2);
  if (log === undefined || rest.length > 0) {
    console.error("usage: bun model-standin.ts <log file>");
    process.exit(2);
  }
  const cleanup: Cleanup = [];
  let turns = 0;
  const { base } = serveStandin(cleanup, (url, _body, request) => {
    const messages = request.method === "POST" && url.pathname === MESSAGES;
    if (messages) turns += 1;
    appendFileSync(
      log,
      `${JSON.stringify({
        at: new Date().toISOString(),
        method: request.method,
        path: url.pathname,
        answered: messages ? turns : null,
      })}\n`
    );
    if (messages) {
      return new Response(
        messageStream([{ type: "text", text: MODEL_STANDIN_MESSAGE }], `msg_standin_${turns}`),
        { headers: { "content-type": "text/event-stream" } }
      );
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
