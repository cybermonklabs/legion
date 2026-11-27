#!/usr/bin/env bun
/**
 * The Envoy listener a case run's pane is given (its ENVOY_URL): a front of the batch's real
 * listener that forwards each request to it as it came, its method, path, query, headers and body,
 * and gives back the listener's answer as the listener gave it, but one. A send
 * (`POST /v1/messages/send`) the listener answers 404 because it has heard of no such session is
 * answered as the listener answers a send to a live session it holds (cmd/listener/api.go
 * sendHandler): 200, the envelope the listener stores for it (contracts.AgentSendEnvelope over
 * MessageEnvelope: on the subject `notifications.agent.<session>`, under the dedupe key
 * `agent.<session>.<idempotency key>`, the message's first line as its summary and the whole text
 * as its payload where the two differ), its recipient, `recipient_state: live` and
 * `state: connected`.
 *
 * A case world's sessions (the author of a handoff, an architect's session) are sessions of no
 * listener the rig runs, so the listener refuses every send to one, and no run could show a send
 * that production delivers. Every check the listener makes of a send before it looks its target up
 * stays the listener's own: the credential, the source session, the message and its metadata. A
 * send the listener refuses for any of them, or delivers to a session it does hold, is answered as
 * the listener answered it. Each send appends one line to the log, the run's calls.jsonl, in the
 * shape of a line of rig.sh's other stand-ins: `{at, as: "envoy", route, target, status, exit,
 * by}`, where `status` is the status the pane was answered, `exit` is 0 for a status below 400 and
 * 1 otherwise (daemon-standin.ts reads the `exit` of every line), and `by` is `listener` or
 * `stand-in`.
 *
 * Usage: `bun envoy-standin.ts <listener URL> <log>`. Prints `listening on http://127.0.0.1:<port>`
 * once the socket is open, on a port the kernel chose.
 */
import { randomUUID } from "node:crypto";
import { appendFile } from "node:fs/promises";

const USAGE = "usage: bun envoy-standin.ts <listener URL> <log>";

const [upstreamArg, logFile, ...rest] = Bun.argv.slice(2);
if (upstreamArg === undefined || logFile === undefined || rest.length > 0) {
  console.error(USAGE);
  process.exit(2);
}
const upstream = upstreamArg.replace(/\/+$/, "");
if (!URL.canParse(upstream)) {
  console.error(`the listener ${upstreamArg} is no URL; ${USAGE}`);
  process.exit(2);
}

/** The send route, which the front answers itself for a session the listener has never heard of. */
const SEND = "/v1/messages/send";

/** Headers of one hop that a proxy does not hand on, and `host`, which names the front. */
const HOP_HEADERS = [
  "connection",
  "keep-alive",
  "proxy-connection",
  "transfer-encoding",
  "upgrade",
  "te",
  "trailer",
  "host",
];

/** The listener's answer to a request of `method` to `url` with `headers` and `body`, its status,
 * headers and body as it gave them. */
async function forward(
  method: string,
  url: URL,
  headers: Headers,
  body: ArrayBuffer | undefined
): Promise<Response> {
  const sent = new Headers(headers);
  for (const name of HOP_HEADERS) sent.delete(name);
  let answered: Response;
  try {
    answered = await fetch(`${upstream}${url.pathname}${url.search}`, {
      method,
      headers: sent,
      body,
      redirect: "manual",
      decompress: false,
    });
  } catch (error) {
    return Response.json(
      { error: `the front could not reach the run's Envoy listener at ${upstream}: ${error}` },
      { status: 502 }
    );
  }
  const back = new Headers(answered.headers);
  for (const name of [...HOP_HEADERS, "content-length"]) back.delete(name);
  return new Response(answered.body, {
    status: answered.status,
    statusText: answered.statusText,
    headers: back,
  });
}

/** The first line of `text` that is not blank, cut to 160 characters with an ellipsis, as the
 * listener's contracts.OneLineSummary gives an envelope's summary. */
function oneLineSummary(text: string): string {
  const line = text.split("\n").find((each) => each.trim() !== "") ?? "";
  const chars = [...line];
  return chars.length <= 160 ? line : `${chars.slice(0, 159).join("")}…`;
}

const text = (value: unknown) => (typeof value === "string" ? value : undefined);

/** The listener's answer to a send of `body` to `target`, a live session it holds: the envelope it
 * stores for it (MessageEnvelope), the recipient, its state and the request's own expects_reply. */
function delivered(body: Record<string, unknown>, target: string) {
  const message = text(body.message) ?? "";
  const summary = oneLineSummary(message);
  const payload = text(body.payload) ?? (message === summary ? undefined : message);
  const key = text(body.idempotency_key) ?? randomUUID();
  const now = Date.now();
  const optional = Object.fromEntries(
    ["source_session", "expires_at", "in_reply_to", "supersedes", "urgency", "expects_reply"]
      .filter((name) => body[name] !== undefined && body[name] !== null)
      .map((name) => [name, body[name]])
  );
  return {
    event_id: randomUUID(),
    source: text(body.source) ?? "agent",
    source_event_id: randomUUID(),
    topic: `notifications.agent.${target}`,
    dedupe_key: `agent.${target}.${key}`,
    issued_at: now,
    payload_summary: summary,
    ...(payload === undefined ? {} : { payload }),
    trace_id: randomUUID(),
    ...optional,
    recipient: target,
    recipient_state: "live",
    state: "connected",
    last_seen: now,
  };
}

/** Whether the listener's answer to a send is its 404 for a session it has never heard of
 * (cmd/listener's noLiveSession and unknownSession: `no live session <id>`). */
async function unknownTarget(answer: Response): Promise<boolean> {
  if (answer.status !== 404) return false;
  const refusal: unknown = await answer
    .clone()
    .json()
    .catch(() => undefined);
  const error =
    typeof refusal === "object" && refusal !== null ? Reflect.get(refusal, "error") : "";
  return typeof error === "string" && error.startsWith("no live session ");
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  // A send's confirmed delivery can wait on the listener longer than Bun's ten idle seconds.
  idleTimeout: 0,
  async fetch(request) {
    const url = new URL(request.url);
    const body =
      request.method === "GET" || request.method === "HEAD"
        ? undefined
        : await request.arrayBuffer();
    const answer = await forward(request.method, url, request.headers, body);
    if (request.method !== "POST" || url.pathname !== SEND) return answer;
    let sent: Record<string, unknown> = {};
    try {
      const parsed: unknown = JSON.parse(new TextDecoder().decode(body));
      if (typeof parsed === "object" && parsed !== null) sent = parsed as Record<string, unknown>;
    } catch {
      // The listener named what is wrong with a body that is no JSON, and its answer stands.
    }
    const target = text(sent.target_session)?.trim() ?? "";
    const standIn = target !== "" && (await unknownTarget(answer));
    const given = standIn ? Response.json(delivered(sent, target)) : answer;
    const line = {
      at: new Date().toISOString(),
      as: "envoy",
      route: `POST ${SEND}`,
      target,
      status: given.status,
      exit: given.status < 400 ? 0 : 1,
      by: standIn ? "stand-in" : "listener",
    };
    await appendFile(logFile, `${JSON.stringify(line)}\n`);
    return given;
  },
});

console.log(`listening on http://127.0.0.1:${server.port}`);
