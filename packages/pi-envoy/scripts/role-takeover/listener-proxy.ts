#!/usr/bin/env bun
/**
 * listener-proxy.ts — one client instance's HTTP path to the Envoy listener, for
 * `scripts/smoke-role-takeover.sh` only.
 *
 * Every client process in that smoke reaches the listener through a proxy of its own, so the
 * smoke reads what each client sent and what the listener answered it without instrumenting any
 * client code: the Oh My Pi extension and the Claude Code channel server run exactly as they
 * ship. Each request and its answer is appended to `--log` as one JSON line:
 *
 *   {"seq":1,"label":"omp-a","at":1761,"method":"POST","path":"/v1/roles/set","status":409,
 *    "request":{...},"response":{...},"ms":3}
 *
 * `response` is the listener's own JSON as it came, which is what the smoke's readers take fields
 * of (role-takeover/clients.sh). The proxy forwards each client's `Authorization`, and an answer
 * the listener did not write, such as a gateway's page echoing the request back, can hold that
 * credential, so such an answer is logged as one line with it redacted (`logAnswer`).
 *
 * Usage: bun listener-proxy.ts --port <n> --upstream <url> --log <file> --label <name>
 */
import { appendFileSync } from "node:fs";
import { bodyForText } from "@legion/envoy-client/excerpt";

interface Options {
  readonly port: number;
  readonly upstream: string;
  readonly log: string;
  readonly label: string;
}

function parseOptions(argv: readonly string[]): Options {
  const values = new Map<string, string>();
  for (let index = 0; index < argv.length; index += 2) {
    const name = argv[index];
    const value = argv[index + 1];
    if (name === undefined || !name.startsWith("--") || value === undefined) {
      throw new Error(`listener-proxy: expected --name value pairs, got ${argv.join(" ")}`);
    }
    values.set(name.slice(2), value);
  }
  const port = Number(values.get("port"));
  const upstream = values.get("upstream");
  const log = values.get("log");
  const label = values.get("label");
  // 0 asks for any free port, which the ready line names: the proxy's own test starts it so.
  if (!Number.isInteger(port) || port < 0) throw new Error("listener-proxy: --port is required");
  if (upstream === undefined) throw new Error("listener-proxy: --upstream is required");
  if (log === undefined) throw new Error("listener-proxy: --log is required");
  if (label === undefined) throw new Error("listener-proxy: --label is required");
  return { port, upstream: upstream.replace(/\/$/, ""), log, label };
}

const options = parseOptions(process.argv.slice(2));

let seq = 0;

/** The body as the log should carry it: parsed JSON where it is JSON, else the text, else null. */
function logBody(text: string): unknown {
  if (text === "") return null;
  try {
    return JSON.parse(text) as unknown;
  } catch {
    return text;
  }
}

/** The shortest run of a credential's characters that holds it, as the excerpt counts one
 *  (`BEARER_PIECE` of packages/envoy-client/src/excerpt.ts): a credential shorter than this is
 *  held only whole. */
const CREDENTIAL_PIECE = 8;

/** Whether `text` holds a run of `CREDENTIAL_PIECE` characters of any of `credentials`. */
function holdsCredential(text: string, credentials: readonly string[]): boolean {
  return credentials.some((credential) => {
    if (credential === "") return false;
    const length = Math.min(CREDENTIAL_PIECE, credential.length);
    for (let at = 0; at + length <= credential.length; at += 1) {
      if (text.includes(credential.slice(at, at + length))) return true;
    }
    return false;
  });
}

/**
 * The answer as the log carries it: null for an empty body, and the listener's own JSON, an
 * object or a list, parsed. Anything else did not come from the listener, which a gateway or a
 * proxy in its place can answer with a page that echoes the request back, the client's credential
 * in it: text, JSON that is no object or list, and JSON that holds `secret` or `proof`, which the
 * listener never writes back. That is logged as `bodyForText` names it, one line with every run of
 * `secret`, the credential the client's `Authorization` presented, and of `proof`, its DPoP proof,
 * redacted, and any `Authorization`, `Bearer` or `DPoP` value.
 */
function logAnswer(text: string, secret: string, proof: string): unknown {
  if (text === "") return null;
  try {
    const parsed: unknown = JSON.parse(text);
    if (typeof parsed === "object" && parsed !== null && !holdsCredential(text, [secret, proof])) {
      return parsed;
    }
  } catch {
    // Not JSON, so not the listener's.
  }
  return bodyForText(text, [secret, proof], "the listener's JSON");
}

function record(entry: Record<string, unknown>): void {
  appendFileSync(options.log, `${JSON.stringify(entry)}\n`);
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: options.port,
  idleTimeout: 0,
  async fetch(request: Request): Promise<Response> {
    const url = new URL(request.url);
    const body = request.method === "GET" || request.method === "HEAD" ? "" : await request.text();
    const headers = new Headers(request.headers);
    headers.delete("host");
    headers.delete("content-length");
    const started = Date.now();
    seq += 1;
    const entry = {
      seq,
      label: options.label,
      // The wall clock the smoke orders one client's requests against another's by.
      at: started,
      method: request.method,
      path: url.pathname,
      query: url.search,
      request: logBody(body),
    };
    let answer: Response;
    try {
      answer = await fetch(`${options.upstream}${url.pathname}${url.search}`, {
        method: request.method,
        headers,
        ...(body === "" ? {} : { body }),
      });
    } catch (error) {
      record({ ...entry, status: 0, response: String(error), ms: Date.now() - started });
      return Response.json({ error: "the listener did not answer the proxy" }, { status: 502 });
    }
    const text = await answer.text();
    // The credential the client presented, after its scheme (a bearer, a DPoP-bound token, Basic's
    // user and password), and its DPoP proof: what the proxy forwarded and an echo can hold.
    const secret = (headers.get("authorization") ?? "").trim().replace(/^\S+\s+/, "");
    const proof = (headers.get("dpop") ?? "").trim();
    const response = logAnswer(text, secret, proof);
    record({ ...entry, status: answer.status, response, ms: Date.now() - started });
    const answerHeaders = new Headers(answer.headers);
    answerHeaders.delete("content-encoding");
    answerHeaders.delete("content-length");
    return new Response(text === "" ? null : text, {
      status: answer.status,
      headers: answerHeaders,
    });
  },
});

process.stdout.write(`listener-proxy ${options.label} ready on ${server.port}\n`);
