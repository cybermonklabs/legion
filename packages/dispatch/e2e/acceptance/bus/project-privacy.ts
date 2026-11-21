#!/usr/bin/env bun
/**
 * bus/project-privacy.ts serves `GET /v1/project-privacy`, the privacy answer a listener in required
 * mode reads at each decision (ENVOY_PROJECT_PRIVACY_URL), for project-access-bus.sh, in one of three
 * modes, and keeps one JSON line per request it answered in --log, `{"caller", "status"}`, which is
 * how the driver counts the listener's privacy reads:
 *
 *   --dispatch <url> --dispatch-token-file <file>
 *       the stand-in for the broker's own route until it exists (plan E's t-e-63), doing what that
 *       route does: it answers only a caller --caller names (the listener's platform token) and
 *       refuses every other 403 `not_listener`; for a listener it reads Dispatch's
 *       `GET /api/v1/access/private-projects` afresh, with the token in --dispatch-token-file (an
 *       account DISPATCH_ACCESS_ACCOUNTS lists, as the broker's is), keeping nothing between calls,
 *       and answers 200 with Dispatch's body unchanged; any failure, a body that is not that answer,
 *       or a project without `slack_channels` is 502 `dispatch_unavailable`, never an empty list.
 *       Past 64 calls at once it answers 503 `busy`.
 *   --forward <broker url>
 *       once the broker serves the route: each request goes to the broker's `/v1/project-privacy`
 *       with the caller's own Authorization header, and the broker's answer comes back unchanged;
 *       this process only counts.
 *   --uncounted
 *       the control of the driver's count: every request is answered `{"projects":[]}`, as a reader
 *       that sends no request answers, and nothing is read or logged, so a listener pointed here
 *       makes no counted read.
 *
 * Usage: bun project-privacy.ts --port-file <file> --log <file> [--caller <name>=<token file>]...
 *          (--dispatch <url> --dispatch-token-file <file> | --forward <url> | --uncounted)
 *
 * It listens on 127.0.0.1 on a port the kernel chooses and writes that port to --port-file once it
 * serves. A caller is named by the token it presents: the first --caller whose file holds the
 * bearer, read afresh at each request, as a projected token is rewritten; any other is `unknown`.
 */
import { appendFileSync, readFileSync, renameSync, writeFileSync } from "node:fs";
import { z } from "zod";

/** Dispatch's private-projects answer: every project with its channels, other members kept. */
const PrivateProjectsAnswer = z.looseObject({
  projects: z.array(z.looseObject({ project: z.string(), slack_channels: z.array(z.string()) })),
});

interface Options {
  portFile: string;
  log: string;
  callers: { name: string; file: string }[];
  mode: "dispatch" | "forward" | "uncounted";
  target: string;
  dispatchTokenFile: string;
}

function refuse(message: string): never {
  console.error(`project-privacy: ${message}`);
  process.exit(2);
}

function parse(args: string[]): Options {
  const options: Options = {
    portFile: "",
    log: "",
    callers: [],
    mode: "uncounted",
    target: "",
    dispatchTokenFile: "",
  };
  const modes: string[] = [];
  for (let i = 0; i < args.length; i++) {
    const flag = args[i];
    const value = (): string => {
      const next = args[++i];
      if (next === undefined) refuse(`${flag} takes a value`);
      return next;
    };
    switch (flag) {
      case "--port-file":
        options.portFile = value();
        break;
      case "--log":
        options.log = value();
        break;
      case "--caller": {
        const [name, file] = value().split("=", 2);
        if (!name || !file) refuse("--caller takes <name>=<token file>");
        options.callers.push({ name, file });
        break;
      }
      case "--dispatch":
        modes.push("dispatch");
        options.target = value();
        break;
      case "--dispatch-token-file":
        options.dispatchTokenFile = value();
        break;
      case "--forward":
        modes.push("forward");
        options.target = value();
        break;
      case "--uncounted":
        modes.push("uncounted");
        break;
      default:
        refuse(`unknown argument ${flag}`);
    }
  }
  if (options.portFile === "" || options.log === "") refuse("--port-file and --log are required");
  if (modes.length !== 1) refuse("give one of --dispatch, --forward and --uncounted");
  options.mode = modes[0] as Options["mode"];
  if (options.mode === "dispatch" && options.dispatchTokenFile === "") {
    refuse("--dispatch needs --dispatch-token-file");
  }
  if (options.mode === "dispatch" && options.callers.length === 0) {
    refuse("--dispatch needs a --caller: the route answers listeners alone");
  }
  return options;
}

const options = parse(process.argv.slice(2));
writeFileSync(options.log, "");

/** The --caller whose token file holds `bearer`, or `unknown`. */
function callerOf(request: Request): string {
  const header = request.headers.get("authorization") ?? "";
  const bearer = header.replace(/^(Bearer|DPoP) /i, "").trim();
  if (bearer === "") return "unknown";
  for (const { name, file } of options.callers) {
    try {
      if (readFileSync(file, "utf8").trim() === bearer) return name;
    } catch {
      // A token file that cannot be read names no caller.
    }
  }
  return "unknown";
}

function answer(status: number, body: unknown): Response {
  return new Response(typeof body === "string" ? body : JSON.stringify(body), {
    status,
    headers: { "content-type": "application/json" },
  });
}

const BUSY_LIMIT = 64;
let inFlight = 0;

async function fromDispatch(): Promise<Response> {
  try {
    const token = readFileSync(options.dispatchTokenFile, "utf8").trim();
    const read = await fetch(`${options.target}/api/v1/access/private-projects`, {
      headers: { authorization: `Bearer ${token}` },
      signal: AbortSignal.timeout(5_000),
    });
    const text = await read.text();
    if (read.status !== 200 || !PrivateProjectsAnswer.safeParse(JSON.parse(text)).success) {
      return answer(502, { code: "dispatch_unavailable" });
    }
    return answer(200, text);
  } catch {
    return answer(502, { code: "dispatch_unavailable" });
  }
}

async function forwarded(request: Request): Promise<Response> {
  const headers: Record<string, string> = {};
  const authorization = request.headers.get("authorization");
  if (authorization !== null) headers.authorization = authorization;
  const dpop = request.headers.get("dpop");
  if (dpop !== null) headers.dpop = dpop;
  try {
    const read = await fetch(`${options.target}/v1/project-privacy`, {
      headers,
      signal: AbortSignal.timeout(10_000),
    });
    return answer(read.status, await read.text());
  } catch {
    return answer(502, { code: "broker_unreachable" });
  }
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: 0,
  async fetch(request) {
    const url = new URL(request.url);
    if (request.method !== "GET" || url.pathname !== "/v1/project-privacy") {
      return answer(404, { code: "not_found" });
    }
    if (options.mode === "uncounted") return answer(200, { projects: [] });
    const caller = callerOf(request);
    let response: Response;
    if (options.mode === "forward") {
      response = await forwarded(request);
    } else if (caller === "unknown") {
      response = answer(403, { code: "not_listener" });
    } else if (inFlight >= BUSY_LIMIT) {
      response = answer(503, { code: "busy" });
    } else {
      inFlight++;
      try {
        response = await fromDispatch();
      } finally {
        inFlight--;
      }
    }
    appendFileSync(options.log, `${JSON.stringify({ caller, status: response.status })}\n`);
    return response;
  },
});

// The port file is written whole, under another name first, so a reader never sees half of it.
writeFileSync(`${options.portFile}.part`, `${server.port}\n`);
renameSync(`${options.portFile}.part`, options.portFile);
console.log(`project-privacy listening on 127.0.0.1:${server.port} (${options.mode})`);
