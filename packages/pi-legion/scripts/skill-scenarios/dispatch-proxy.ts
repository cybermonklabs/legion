#!/usr/bin/env bun
/**
 * The proxy between a run's agents and the run's own Dispatch server. Each request goes to the
 * upstream server as it came, its method, path, query, headers and body, and its answer comes back
 * as the server gave it, its status, headers and body, compressed or not. The one exception is the
 * read of earlier plans: with `--plans`, `GET /api/v1/projects/<project>/plans` is answered, whatever
 * its query, with the recorded answer of a real Dispatch (the plan ratings proof's capture), every
 * whole key of the key map replaced by its value. That request never reaches the upstream server.
 * `/api/v1/issues`, a plans read of another project and every other request go through.
 *
 * Every request appends one line, `{at, method, path, query, status}` (`DispatchProxyLine`), to the
 * log: the run's record of what the agents asked of Dispatch. `query` is the raw query without its
 * `?`, and `""` for none.
 *
 * The recording's source is `plans-answer.source` beside `<answer>` (`PlansAnswerSource`): the
 * commit it was captured at, the sha256 of the recorded body, and the map from its project key and
 * issue keys to the run's. The proxy refuses to start, exit 2 naming what is wrong, unless the
 * recording is held to its source as scored-runs-contracts.ts's readRecordedPlans holds it: its
 * sha256 is the source's (both sums named), it is a plans answer in JSON, and the map takes the
 * recording's project to `<project>` and each of its issues to a key of `<project>`. A key is
 * replaced where it is whole (mapRecordedKeys: no letter, digit, `-` or `_` stands beside it), so
 * a key that begins another (`P-1` and `P-12`) replaces only itself. Only the strings that hold a
 * key are written again; every other byte of the recording is served as recorded.
 *
 * Usage: `bun dispatch-proxy.ts <port> <upstream> <log> [--plans <answer> <project>]`
 * Prints `listening on http://127.0.0.1:<port>` once the socket is open (port 0: the kernel's
 * pick).
 */
import { appendFile } from "node:fs/promises";
import {
  type DispatchProxyLine,
  mapRecordedKeys,
  type RecordedPlansFiles,
  readRecordedPlans,
} from "./scored-runs-contracts";

const USAGE = "usage: bun dispatch-proxy.ts <port> <upstream> <log> [--plans <answer> <project>]";

function refuse(message: string): never {
  console.error(`dispatch-proxy: ${message}`);
  process.exit(2);
}

const [portArg, upstreamArg, logFile, ...rest] = Bun.argv.slice(2);
if (portArg === undefined || upstreamArg === undefined || logFile === undefined) refuse(USAGE);
if (rest.length !== 0 && (rest.length !== 3 || rest[0] !== "--plans")) refuse(USAGE);
/** The upstream server's root, with no trailing `/`, which each request's path follows. */
const upstream = upstreamArg.replace(/\/+$/, "");
if (!URL.canParse(upstream)) refuse(`the upstream ${upstreamArg} is no URL; ${USAGE}`);

/** The JSON string tokens of a JSON text: each one whole, its quotes and escapes as written. */
const STRING_TOKEN = /"(?:[^"\\]|\\.)*"/g;

/**
 * The recorded answer with every whole key of `keyMap` replaced by its value (mapRecordedKeys).
 * Each JSON string of `raw` that holds a key is decoded, its keys replaced, and encoded again;
 * every other string, and every byte between strings, stays as recorded.
 */
function mapKeys(raw: string, keyMap: Readonly<Record<string, string>>): string {
  return raw.replace(STRING_TOKEN, (token) => {
    const text: unknown = JSON.parse(token);
    if (typeof text !== "string") return token;
    const mapped = mapRecordedKeys(text, keyMap);
    return mapped === text ? token : JSON.stringify(mapped);
  });
}

/** The plans read the proxy answers, and the body it answers it with; undefined without
 *  `--plans`. */
const plans = rest.length === 0 ? undefined : recordedPlans(rest[1] ?? "", rest[2] ?? "");

function recordedPlans(
  answerFile: string,
  project: string
): { readonly path: string; readonly body: string } {
  let recording: RecordedPlansFiles;
  try {
    recording = readRecordedPlans(answerFile, project);
  } catch (error) {
    refuse(`the recording ${error instanceof Error ? error.message : String(error)}`);
  }
  return {
    path: `/api/v1/projects/${encodeURIComponent(project)}/plans`,
    body: mapKeys(recording.raw.toString("utf8"), recording.source.keyMap),
  };
}

/** Headers of one hop that a proxy does not hand on, and `host`, which names the proxy. */
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

/** The upstream server's answer to `request`, as it gave it. */
async function forward(request: Request, url: URL): Promise<Response> {
  const headers = new Headers(request.headers);
  for (const name of HOP_HEADERS) headers.delete(name);
  const body =
    request.method === "GET" || request.method === "HEAD" ? undefined : await request.arrayBuffer();
  let answered: Response;
  try {
    answered = await fetch(`${upstream}${url.pathname}${url.search}`, {
      method: request.method,
      headers,
      body,
      redirect: "manual",
      decompress: false,
    });
  } catch (error) {
    return Response.json(
      { error: `the proxy could not reach the run's Dispatch at ${upstream}: ${error}` },
      { status: 502 }
    );
  }
  // The body goes back as the server sent it, compressed or not, with its encoding header; its
  // length goes with the hop's own headers, since the streamed body is framed again.
  const back = new Headers(answered.headers);
  for (const name of [...HOP_HEADERS, "content-length"]) back.delete(name);
  return new Response(answered.body, {
    status: answered.status,
    statusText: answered.statusText,
    headers: back,
  });
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: Number(portArg),
  // A Dispatch read can wait on the server longer than Bun's default of ten idle seconds.
  idleTimeout: 0,
  async fetch(request) {
    const url = new URL(request.url);
    const response =
      plans !== undefined && request.method === "GET" && url.pathname === plans.path
        ? new Response(plans.body, { headers: { "Content-Type": "application/json" } })
        : await forward(request, url);
    const line: DispatchProxyLine = {
      at: new Date().toISOString(),
      method: request.method,
      path: url.pathname,
      query: url.search.replace(/^\?/, ""),
      status: response.status,
    };
    await appendFile(logFile, `${JSON.stringify(line)}\n`);
    return response;
  },
});

console.log(`listening on http://127.0.0.1:${server.port}`);
