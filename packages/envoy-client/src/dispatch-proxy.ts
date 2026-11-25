import { lstatSync, rmSync } from "node:fs";
import {
  createServer,
  type IncomingHttpHeaders,
  type IncomingMessage,
  type Server,
  type ServerResponse,
} from "node:http";
import { constants } from "node:os";
import type { Duplex } from "node:stream";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import { brokerTokenFor } from "./broker-credential";
import type { ActiveDispatchConfig } from "./dispatch-config";
import { DISPATCH_TOOL_DEADLINE_MS, tokenNotLive } from "./dispatch-http";
import { proxyExitLine } from "./dispatch-proxy-command";
import { hasErrnoCode, messageFor } from "./errors";

/**
 * `dispatch proxy --socket <path> -- <command>` (dispatch://INFRA-4799/spec, R1-R6): a local
 * proxy to Dispatch for the life of one command. It listens on a unix socket, sends a GET under
 * `/api/` and a comment POST that carries an `Idempotency-Key` to the Dispatch server this
 * session's `dispatch` command resolves, with the session's one broker credential, and gives back
 * Dispatch's answer as Dispatch sent it. `dispatch-cli.ts` resolves the server, the session and
 * the credential as every spec command does, then calls {@link runDispatchProxy}; the parse of its
 * arguments and its help are `dispatch-proxy-command.ts`'s.
 *
 * The standard practice it follows: a local authenticating proxy holds the credential and signs
 * each request, so a tool holds none and the broker sees one client for the session, which trades
 * once and renews by the one client's rule rather than once for each request.
 */

/** What {@link runDispatchProxy} is given. */
export interface DispatchProxyRun {
  /** The socket's path, absolute. */
  readonly socket: string;
  /** The command and its arguments. */
  readonly command: readonly string[];
  /**
   * The environment the Dispatch configuration was resolved from, empty variables dropped. The
   * first trade reads it too, so it trades through the one client that `config.credential`
   * presents (`broker-credential.ts`, `CLIENTS`, keyed by the session and these variables).
   */
  readonly env: Readonly<Record<string, string>>;
  /** The environment the command runs with, as the proxy was given it; `DISPATCH_PROXY_SOCKET`
   *  is added to it. */
  readonly commandEnv: Readonly<Record<string, string>>;
  /** Where the command runs. */
  readonly cwd: string;
  /** Dispatch under `DISPATCH_TOKEN_BROKER=1`: its URL and the session's broker credential. */
  readonly config: ActiveDispatchConfig;
  readonly sessionId: string;
  /** Where each line of the proxy's own goes: its causes, and its exit line. */
  readonly stderr: (text: string) => void;
}

/** The status the proxy exits with when it cannot start (R6), as every usage error of the
 *  `dispatch` command does. */
const CANNOT_START = 2;
/** A command that does not exist, and one that exists and cannot run, as a shell answers them
 *  (`agent-secrets register --exec`, `cmd/agent-secrets/main.go`). */
const COMMAND_NOT_FOUND = 127;
const COMMAND_CANNOT_RUN = 126;

/** The signals the proxy sends on to its command. SIGHUP is one of them: a hangup reaches the
 *  proxy whenever its terminal goes (a tmux pane or an ssh session that closes), and a proxy that
 *  died of it would leave its command running and its socket on disk. */
const FORWARDED = ["SIGTERM", "SIGINT", "SIGHUP"] as const;
type Forwarded = (typeof FORWARDED)[number];

/** The proxy's own refusals (R5), each with the shape of Dispatch's own: `{code, error}`. No code
 *  of Dispatch's starts with `PROXY_`, so a tool tells the two apart. */
type ProxyRefusal =
  | { readonly status: 400; readonly code: "PROXY_TARGET_REFUSED" }
  | { readonly status: 405; readonly code: "PROXY_METHOD_REFUSED" }
  | { readonly status: 502; readonly code: "PROXY_TRADE_REFUSED" | "PROXY_DISPATCH_UNREACHABLE" };

const TARGET_REFUSED = { status: 400, code: "PROXY_TARGET_REFUSED" } as const;
const METHOD_REFUSED = { status: 405, code: "PROXY_METHOD_REFUSED" } as const;
const TRADE_REFUSED = { status: 502, code: "PROXY_TRADE_REFUSED" } as const;
const DISPATCH_UNREACHABLE = { status: 502, code: "PROXY_DISPATCH_UNREACHABLE" } as const;

/** What the proxy sends, as each of its refusals of a request says it. */
const SENDS =
  "The proxy sends a GET whose path starts with /api/, and a POST to /api/v1/issues/<key>/comments that carries an Idempotency-Key header, and nothing else.";

/** The headers of one hop that are copied neither way (RFC 9110, section 7.6.1), beside the
 *  ones a `Connection` header names. */
const HOP_BY_HOP = new Set([
  "connection",
  "keep-alive",
  "proxy-connection",
  "proxy-authenticate",
  "proxy-authorization",
  "te",
  "trailer",
  "transfer-encoding",
  "upgrade",
]);

/**
 * The request headers the proxy does not copy beside the hop-by-hop ones: the tool's `Host`,
 * since the request goes to Dispatch's; `Expect`, which asks this hop for a 100 Continue;
 * `Authorization` and `DPoP`, which the session's credential replaces; `Accept-Encoding`, which is
 * set to `identity`; and `Content-Length`, which `fetch` writes for the body it sends.
 */
const NOT_COPIED = new Set([
  ...HOP_BY_HOP,
  "host",
  "expect",
  "authorization",
  "dpop",
  "accept-encoding",
  "content-length",
]);

/** The one write the proxy sends (R2). */
const COMMENT_PATH = /^\/api\/v1\/issues\/[^/]+\/comments$/;

/**
 * The dispatch command that sends each other write of the `dispatch` commands: its name, the
 * method, and the route Dispatch serves the write at (`routes_table.go`), where each `{…}` stands
 * for one segment of the path. A refusal names the command of the row that has the method of the
 * request, and whose route the path of the request fills. No `dispatch` command sends a write that
 * no row holds, so its refusal names no command. The tool specs (`dispatchToolSpecs`) give no
 * method or route for a command, so the rows are written here. `dispatch-proxy.test.ts` sends each
 * write of `DispatchClient` through the proxy and reads the name in each refusal.
 *
 * Three names are not the obvious ones. `dispatch suggest` sends the comment routes too, which the
 * rows name `comment`. `dispatch message` uploads the pictures of a reply to a direct message to
 * the agent's artifacts. `dispatch artifact` checks an inline text with the markdown round trip
 * before it uploads the text. The second sync of `architecture-sync` is the route of a Dispatch
 * older than the first, which `DispatchClient` asks when the first has no route. The POST of
 * {@link COMMENT_PATH} has no row: the proxy sends it when it carries a key, and its refusal
 * without a key names `dispatch comment`.
 */
const WRITE_COMMANDS: readonly (readonly [command: string, method: string, route: string])[] = [
  ["issue", "POST", "/api/v1/issues"],
  ["issue-update", "PATCH", "/api/v1/issues/{key}"],
  ["issue-update", "POST", "/api/v1/issues/{key}/status-change"],
  ["claim", "POST", "/api/v1/issues/{key}/claim"],
  ["claim", "DELETE", "/api/v1/issues/{key}/claim"],
  ["comment", "POST", "/api/v1/artifacts/{id}/comments"],
  ["resolve-comment", "POST", "/api/v1/comments/{id}/resolve"],
  ["edit-comment", "PATCH", "/api/v1/comments/{id}"],
  ["delete-comment", "DELETE", "/api/v1/comments/{id}"],
  ["message", "POST", "/api/v1/issues/{key}/messages"],
  ["message", "POST", "/api/v1/messages/{id}/reply"],
  ["message", "POST", "/api/v1/agents/{session_id}/artifacts"],
  ["edit-message", "PATCH", "/api/v1/messages/{id}"],
  ["delete-message", "DELETE", "/api/v1/messages/{id}"],
  ["send-to-agent", "POST", "/api/v1/agents/{session_id}/messages"],
  ["ask", "POST", "/api/v1/issues/{key}/asks"],
  ["ask", "POST", "/api/v1/artifacts/{id}/asks"],
  ["edit-ask", "PATCH", "/api/v1/asks/{id}"],
  ["resolve-ask", "POST", "/api/v1/asks/{id}/resolve"],
  ["follow", "PUT", "/api/v1/asks/{id}/followers/{session_id}"],
  ["follow", "DELETE", "/api/v1/asks/{id}/followers/{session_id}"],
  ["result", "POST", "/api/v1/issues/{key}/results"],
  ["result", "POST", "/api/v1/issues/{key}/results/dry-run"],
  ["result", "POST", "/api/v1/results/{id}/retract"],
  ["handoff-write", "POST", "/api/v1/issues/{key}/handoffs/{phase}"],
  ["artifact", "POST", "/api/v1/issues/{key}/artifacts"],
  ["artifact", "POST", "/api/v1/projects/{key}/artifacts"],
  ["artifact", "POST", "/api/v1/markdown/round-trip"],
  ["doc-edit", "POST", "/api/v1/artifacts/{id}/edits"],
  ["request-approval", "POST", "/api/v1/artifacts/{id}/approval-requests"],
  ["plan-approve", "POST", "/api/v1/artifacts/{id}/plan-reviews"],
  ["review", "POST", "/api/v1/artifacts/{id}/versions/{number}/reviews"],
  ["project", "POST", "/api/v1/projects"],
  ["related", "POST", "/api/v1/projects/{key}/related"],
  ["architecture-sync", "POST", "/api/v1/projects/{key}/architecture-sources/sync"],
  ["architecture-sync", "POST", "/api/v1/projects/{key}/architecture-source/sync"],
  ["architecture-sync", "POST", "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync"],
];

/** Each row of {@link WRITE_COMMANDS}, with its route cut at each `/` as a path is. */
const WRITE_ROUTES = WRITE_COMMANDS.map(([command, method, route]) => ({
  command,
  method,
  route: route.split("/"),
}));

/** What the exit line counts (`proxyExitLine`, R4). */
class Tally {
  /** Each request sent to Dispatch that it answered, the resend after `BROKER_TOKEN_NOT_ACTIVE`
   *  included, so it equals what a server that counts them counts. */
  requests = 0;
  /** Each access token a trade gave, the first trade's included: one for each trade that gave
   *  one, since a refused trade gives none. */
  readonly tokens = new Set<string>();
  /** The trades that gave a token for the resend after a `BROKER_TOKEN_NOT_ACTIVE` answer. */
  renewed = 0;

  line(): string {
    return proxyExitLine(this.requests, this.tokens.size, this.renewed);
  }
}

/**
 * Why the proxy cannot make its socket at `socket`, or undefined where nothing is there. A path
 * that exists is refused, a socket a killed proxy left behind included (R1): the proxy removes
 * only the socket it made, so it never takes over or removes another's file.
 */
export function socketPathTaken(socket: string): string | undefined {
  try {
    lstatSync(socket);
  } catch (error) {
    if (hasErrnoCode(error, "ENOENT")) return undefined;
    return `dispatch proxy: the socket path ${socket} cannot be read: ${messageFor(error)}`;
  }
  return `dispatch proxy: the socket path ${socket} exists. The proxy makes its socket new and refuses a path that exists, a socket a killed proxy left behind included. Give it a path in a new directory (mktemp -d).`;
}

/**
 * Runs the proxy for the life of `run.command` and answers the status to exit with (R4, R6). It
 * refuses a socket path that exists; trades for the session's token, waiting out a broker that is
 * busy or a pod session not yet registered within `DISPATCH_TOOL_DEADLINE_MS`; listens on the
 * socket with mode 0600; starts the command with `DISPATCH_PROXY_SOCKET` set and the proxy's own
 * standard input, output and error; sends SIGTERM, SIGINT and SIGHUP on to it and serves until it
 * ends; then closes the server, removes the socket and writes the exit line on standard error as
 * its last line. Each cause it cannot start for is written first and answers 2, and a signal
 * before the command starts ends it at once with 128 plus the signal's number. It writes nothing
 * to the session's directory, and never reads its own standard input.
 */
export async function runDispatchProxy(run: DispatchProxyRun): Promise<number> {
  const say = (line: string): void => run.stderr(`${line}\n`);
  const tally = new Tally();

  // A signal before the command starts ends the run at once; one after it goes to the command.
  const beforeStart = new AbortController();
  let caught: Forwarded | undefined;
  let child: Bun.Subprocess | undefined;
  const handlers = FORWARDED.map((signal) => {
    const handler = (): void => {
      if (child === undefined) {
        caught ??= signal;
        beforeStart.abort(new Error(`${signal} arrived before the command started`));
        return;
      }
      try {
        child.kill(signal);
      } catch {
        // The command has ended already; the proxy is on its way out too.
      }
    };
    process.on(signal, handler);
    return [signal, handler] as const;
  });
  const endedBySignal = (signal: Forwarded): number => {
    say(`dispatch proxy: ${signal} arrived before the command started, so it runs no command.`);
    return 128 + constants.signals[signal];
  };

  const server = proxyServer(run, tally);
  let made = false;
  try {
    const taken = socketPathTaken(run.socket);
    if (taken !== undefined) {
      say(taken);
      return CANNOT_START;
    }
    try {
      const first = await brokerTokenFor(run.env, run.sessionId)(
        AbortSignal.any([AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS), beforeStart.signal]),
        true
      );
      tally.tokens.add(first.token.accessToken);
    } catch (error) {
      if (caught !== undefined) return endedBySignal(caught);
      say(
        `dispatch proxy: the secrets broker gave session ${run.sessionId} no Dispatch token, so the proxy starts no command: ${messageFor(error)}`
      );
      return CANNOT_START;
    }
    if (caught !== undefined) return endedBySignal(caught);
    try {
      await listen(server, run.socket);
      made = true;
    } catch (error) {
      say(`dispatch proxy: it cannot listen on ${run.socket}: ${messageFor(error)}`);
      return CANNOT_START;
    }
    server.on("error", (error) => say(`dispatch proxy: its socket failed: ${messageFor(error)}`));
    if (caught !== undefined) return endedBySignal(caught);

    let started: Bun.Subprocess;
    try {
      started = Bun.spawn([...run.command], {
        cwd: run.cwd,
        env: { ...run.commandEnv, DISPATCH_PROXY_SOCKET: run.socket },
        stdin: "inherit",
        stdout: "inherit",
        stderr: "inherit",
      });
    } catch (error) {
      say(`dispatch proxy: ${run.command[0]} could not be run: ${messageFor(error)}`);
      return hasErrnoCode(error, "ENOENT") ? COMMAND_NOT_FOUND : COMMAND_CANNOT_RUN;
    }
    child = started;
    const exited = await started.exited;
    const signal = started.signalCode;
    return signal === null ? (started.exitCode ?? exited) : 128 + constants.signals[signal];
  } finally {
    for (const [signal, handler] of handlers) process.off(signal, handler);
    if (made) {
      server.close();
      server.closeAllConnections();
      rmSync(run.socket, { force: true });
    }
    say(tally.line());
  }
}

/**
 * Listens on `socket` with the process's umask at 0177 for the call, so the socket is made with
 * mode 0600 (R1): only this user can connect. The umask is the process's, so it is restored as
 * soon as the socket is bound or the bind has failed.
 */
async function listen(server: Server, socket: string): Promise<void> {
  const { promise, resolve, reject } = Promise.withResolvers<void>();
  server.once("error", reject);
  const kept = process.umask(0o177);
  try {
    server.listen(socket, () => resolve());
    await promise;
  } finally {
    process.umask(kept);
    server.off("error", reject);
  }
}

/**
 * The proxy's HTTP server. `node:http` hands a CONNECT and an upgrade to events of their own,
 * which skip the request handler, and a request it cannot parse, the asterisk form (`OPTIONS *`)
 * among them, to `clientError`: each is answered 405 on its socket.
 */
function proxyServer(run: DispatchProxyRun, tally: Tally): Server {
  // Dispatch's URL, parsed once with no fragment, and its own path with no `/` at its end, so `/`
  // alone is the empty path. `serve` sends each request to a copy of this URL.
  const dispatch = new URL(run.config.url);
  dispatch.hash = "";
  const base = withoutTrailingSlashes(dispatch.pathname);
  const server = createServer((request, response) => {
    serve(request, response, run, dispatch, base, tally).catch((error: unknown) => {
      if (!response.headersSent) {
        answerRefusal(response, DISPATCH_UNREACHABLE, messageFor(error));
        return;
      }
      // An answer the proxy cannot finish: Dispatch's broke off, or the tool hung up. Under Bun,
      // destroying the socket sends what is queued on it, then closes the connection, so the tool
      // reads Dispatch's status line, its headers and the body bytes the fetch yielded, then the
      // close; `response.destroy()` would drop what is queued. A response queued behind an earlier
      // answer on a pipelined connection has no socket until its turn, so it is destroyed itself:
      // it drops what it holds, and the tool reads the earlier answer, whole or cut where the tool
      // hung up on it, then the close, and nothing of this one.
      if (response.socket) response.socket.destroy();
      else response.destroy();
    });
  });
  server.on("connect", (_request: IncomingMessage, socket: Duplex) => {
    refuseOnSocket(
      socket,
      `The proxy opens no tunnel (CONNECT). ${SENDS} Read a route with a GET under /api/, or with dispatch api.`
    );
  });
  server.on("upgrade", (_request: IncomingMessage, socket: Duplex) => {
    refuseOnSocket(
      socket,
      `The proxy upgrades no connection (a WebSocket, for one). ${SENDS} Read the route with a plain GET, or with dispatch api.`
    );
  });
  server.on("clientError", (_error: Error, socket: Duplex) => {
    refuseOnSocket(
      socket,
      `The proxy cannot read this request as one it sends, the asterisk form (OPTIONS *) among them. ${SENDS} Read a route with a GET under /api/, or with dispatch api.`
    );
  });
  return server;
}

/** A 405 written on `socket` itself, for a request the server's handler never sees, and the
 *  socket closed after it. */
function refuseOnSocket(socket: Duplex, error: string): void {
  if (!socket.writable) {
    socket.destroy();
    return;
  }
  const body = JSON.stringify({ code: METHOD_REFUSED.code, error });
  socket.end(
    [
      "HTTP/1.1 405 Method Not Allowed",
      "Content-Type: application/json",
      `Content-Length: ${Buffer.byteLength(body)}`,
      "Connection: close",
      "",
      body,
    ].join("\r\n")
  );
}

/** One of the proxy's own refusals, with the JSON shape of Dispatch's own refusals. */
function answerRefusal(response: ServerResponse, refusal: ProxyRefusal, error: string): void {
  const body = JSON.stringify({ code: refusal.code, error });
  response.writeHead(refusal.status, {
    "Content-Type": "application/json",
    "Content-Length": Buffer.byteLength(body),
  });
  response.end(body);
}

/** A target in absolute form (it names a scheme) or one that starts with `//`: each names a
 *  host, and the proxy sends to Dispatch's alone (R2). */
function namesAHost(target: string): boolean {
  return /^[A-Za-z][A-Za-z0-9+.-]*:/.test(target) || target.startsWith("//");
}

/**
 * Whether `path` holds a segment `new URL` would resolve away (`.` or `..`, also with a dot
 * percent-encoded as `%2e`) or a backslash, which it reads as `/`: such a path reaches another
 * route than the one it names before the send (`/api/../metrics` is `/metrics`, which Dispatch
 * serves outside `/api`), so the proxy reads it as no path it sends.
 */
function resolvesElsewhere(path: string): boolean {
  if (path.includes("\\")) return true;
  return path.split("/").some((segment) => {
    const plain = segment.replace(/%2e/gi, ".");
    return plain === "." || plain === "..";
  });
}

/** A `#`, which starts a fragment, or a control character: U+007F, or a code unit below U+0020. */
const FRAGMENT_OR_CONTROL = /[#\u007f]|[^\u0020-\uffff]/;

/**
 * Why the proxy sends nothing for the raw request `target`, whose path is `path`: a path that does
 * not start with `/` or that {@link resolvesElsewhere}, or a target that holds a `#` or a control
 * character. The URL the proxy would send reads each of these another way than the target does: a
 * `#` starts a fragment, which no request sends, and `new URL` drops a tab or a line break. A
 * control character has no place in a request target (RFC 9112, section 3.2). Undefined for a
 * target with none of them.
 */
function elsewhereRefusal(target: string, path: string): string | undefined {
  if (path.startsWith("/") && !resolvesElsewhere(path) && !FRAGMENT_OR_CONTROL.test(target)) {
    return undefined;
  }
  return `${SENDS} The target ${JSON.stringify(target)} reaches another route than the one it names (a dot segment, a backslash, a # or a control character). Name the route's own path, or read it with dispatch api.`;
}

/**
 * Why the proxy sends no `method` to `path`, the path of the URL it would send below Dispatch's
 * own path, naming the dispatch command to use instead where a `dispatch` command sends the
 * request; undefined for the two kinds of request it sends (R2, R5): a GET under `/api/`, and the
 * comment POST that carries an `Idempotency-Key`.
 */
function methodRefusal(method: string, path: string, key: string | undefined): string | undefined {
  const quoted = JSON.stringify(path);
  if (method === "GET") {
    return path.startsWith("/api/")
      ? undefined
      : `${SENDS} ${quoted} is outside /api/. Read a route of /api/v1 with dispatch api --path <path>.`;
  }
  if (method === "POST" && COMMENT_PATH.test(path)) {
    return (key ?? "").trim() !== ""
      ? undefined
      : `${SENDS} This comment POST carries no Idempotency-Key header. Send it with one, so a resend posts the comment once, or post the comment with dispatch comment.`;
  }
  if (method === "POST" || method === "PUT" || method === "PATCH" || method === "DELETE") {
    const named = writeCommand(method, path);
    if (named === undefined) {
      return `${SENDS} ${method} ${quoted} is another write, and no dispatch command sends it, so this refusal names no command to use instead.`;
    }
    return `${SENDS} ${method} ${quoted} is another write. Send it with dispatch ${named}. AskGate reviews that call if it gates the command.`;
  }
  return `${SENDS} ${method} is no method it sends. Read the route with a GET, or with dispatch api.`;
}

/** The dispatch command of the row of {@link WRITE_COMMANDS} that has `method`, and whose route
 *  `path` fills segment for segment; undefined where it fills no row's route. */
function writeCommand(method: string, path: string): string | undefined {
  const segments = path.split("/");
  for (const row of WRITE_ROUTES) {
    if (row.method !== method || row.route.length !== segments.length) continue;
    if (row.route.every((part, at) => fills(part, segments[at] ?? ""))) return row.command;
  }
  return undefined;
}

/** Whether a path's `segment` fills `part` of a route: a `{…}` part takes each segment that is
 *  not empty, and each other part takes only itself. */
function fills(part: string, segment: string): boolean {
  return part.startsWith("{") ? segment !== "" : part === segment;
}

/** A percent-encoded `/` or `\`, in either case. */
const ENCODED_SEPARATOR = /%(?:2f|5c)/i;

/**
 * Why the proxy sends nothing to `url`, the URL it built for a request, checked whole as it would
 * send it: undefined only where its origin is Dispatch's, its path is under Dispatch's own path
 * `base` and `/api/`, it has no fragment, and its path holds no {@link ENCODED_SEPARATOR}. An
 * empty fragment counts too: `hash` reads it as no fragment, and `href` keeps its `#`, which a
 * URL holds nowhere else. Dispatch's server decodes a `%2F` before it routes a path that no API
 * route takes, so `/api/%2F..%2Fmetrics` is `/metrics` there.
 */
function outboundRefusal(url: URL, dispatch: URL, base: string): string | undefined {
  if (
    url.origin === dispatch.origin &&
    url.pathname.startsWith(`${base}/api/`) &&
    !url.href.includes("#") &&
    !ENCODED_SEPARATOR.test(url.pathname)
  ) {
    return undefined;
  }
  return `${SENDS} The URL it would send for ${JSON.stringify(url.pathname.slice(base.length))} is outside /api/ of the Dispatch server, has a fragment, or holds a percent-encoded / or \\ (%2F or %5C, in either case), which Dispatch's server can decode into another route. Name the route's own path, or read it with dispatch api.`;
}

/** The names a `Connection` header lists, each a header of this hop alone. */
function connectionNamed(value: string | null | undefined): Set<string> {
  return new Set(
    (value ?? "")
      .split(",")
      .map((name) => name.trim().toLowerCase())
      .filter((name) => name !== "")
  );
}

/** The tool's end-to-end headers, as the proxy sends them on, with `Accept-Encoding: identity`
 *  so Dispatch answers with no content encoding (R3). */
function forwardedHeaders(incoming: IncomingHttpHeaders): Record<string, string> {
  const named = connectionNamed(incoming.connection);
  const headers: Record<string, string> = {};
  for (const [name, value] of Object.entries(incoming)) {
    if (value === undefined || NOT_COPIED.has(name) || named.has(name)) continue;
    headers[name] = Array.isArray(value) ? value.join(", ") : value;
  }
  headers["accept-encoding"] = "identity";
  return headers;
}

/** Dispatch's end-to-end headers, as the proxy gives them back (R3). Its `Content-Length` comes
 *  back as Dispatch sent it, whatever the `Content-Encoding`: the proxy fetches with
 *  `decompress: false`, so that length frames the very bytes it gives back. An answer that also
 *  carries `Transfer-Encoding` is the one exception: RFC 9112, section 6.3, has an intermediary
 *  remove the received `Content-Length` before it forwards such a message, so it is left out, and
 *  `node:http` sends the body chunked. `Transfer-Encoding` itself is hop-by-hop, and is never
 *  copied. */
function answeredHeaders(headers: Headers): Record<string, string | string[]> {
  const named = connectionNamed(headers.get("connection"));
  // Bun's fetch keeps both headers of such an answer, and reads its body by the transfer coding,
  // past that length.
  const transferCoded = headers.has("transfer-encoding");
  const kept: Record<string, string | string[]> = {};
  headers.forEach((value, name) => {
    if (HOP_BY_HOP.has(name) || named.has(name) || name === "set-cookie") return;
    if (name === "content-length" && transferCoded) return;
    kept[name] = value;
  });
  const cookies = headers.getSetCookie();
  if (cookies.length > 0) kept["set-cookie"] = cookies;
  return kept;
}

/** The request's body, read whole: the resend after `BROKER_TOKEN_NOT_ACTIVE` sends it again. */
async function wholeBody(request: IncomingMessage): Promise<Uint8Array<ArrayBuffer>> {
  const chunks: Buffer[] = [];
  for await (const chunk of request) {
    chunks.push(Buffer.isBuffer(chunk) ? chunk : Buffer.from(chunk));
  }
  const body = new Uint8Array(chunks.reduce((length, chunk) => length + chunk.length, 0));
  let at = 0;
  for (const chunk of chunks) {
    body.set(chunk, at);
    at += chunk.length;
  }
  return body;
}

/** A trade that gave no live token for a request, in the broker's words (R5). */
class TradeRefused extends Error {}

/** One send of a request to Dispatch: the credential's headers it presented and the answer. */
interface Sent {
  readonly presented: Readonly<Record<string, string>>;
  readonly answer: Response;
  /** Whether its token was one no earlier request presented: a trade gave it for this send. */
  readonly fresh: boolean;
}

/**
 * One request through the proxy: the two target rules on the raw target (a host, then another
 * route), the method and path rule on the path of the URL it would send, the check of that URL
 * whole, then the send. `dispatch` is Dispatch's URL as {@link proxyServer} parsed it, and `base`
 * its own path.
 */
async function serve(
  request: IncomingMessage,
  response: ServerResponse,
  run: DispatchProxyRun,
  dispatch: URL,
  base: string,
  tally: Tally
): Promise<void> {
  // The raw request target, as the tool wrote it: `node:http` keeps it unparsed.
  const target = request.url ?? "";
  const method = request.method ?? "";
  if (namesAHost(target)) {
    answerRefusal(
      response,
      TARGET_REFUSED,
      `The proxy sends each request to the Dispatch server ${run.config.url} alone, so a target that names a host, ${JSON.stringify(target)}, is refused. Write the path and the query alone.`
    );
    return;
  }
  const at = target.indexOf("?");
  const path = at === -1 ? target : target.slice(0, at);
  // The URL it sends: a copy of Dispatch's, with only its path and its query set from the target.
  // The target is never parsed as a reference, so nothing in it can change the origin.
  const outbound = new URL(dispatch);
  outbound.pathname = `${base}${path}`;
  outbound.search = at === -1 ? "" : target.slice(at);
  const header = request.headers["idempotency-key"];
  const key = Array.isArray(header) ? header.join(", ") : header;
  // The method and path rule reads the path that the URL holds below Dispatch's own path, and the
  // URL is checked whole last, so the request it sends is the one each rule passed.
  const refusal =
    elsewhereRefusal(target, path) ??
    methodRefusal(method, outbound.pathname.slice(base.length), key) ??
    outboundRefusal(outbound, dispatch, base);
  if (refusal !== undefined) {
    answerRefusal(response, METHOD_REFUSED, refusal);
    return;
  }

  const body = method === "GET" ? undefined : await wholeBody(request);
  // One URL for the proof and for the fetch.
  const url = outbound.href;
  const headers = forwardedHeaders(request.headers);
  // A tool that hangs up stops the trade it waits on, and Dispatch's answer on its way.
  const upstream = new AbortController();
  response.once("close", () => {
    if (!response.writableFinished) upstream.abort();
  });

  const send = async (): Promise<Sent> => {
    let presented: Readonly<Record<string, string>>;
    try {
      presented = await run.config.credential.headers(
        method,
        url,
        undefined,
        AbortSignal.any([AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS), upstream.signal])
      );
    } catch (error) {
      throw new TradeRefused(messageFor(error));
    }
    const token = (presented.Authorization ?? "").replace(/^DPoP /, "");
    const fresh = !tally.tokens.has(token);
    tally.tokens.add(token);
    const answer = await fetch(url, {
      method,
      headers: { ...headers, ...presented },
      ...(body === undefined ? {} : { body }),
      redirect: "manual",
      decompress: false,
      signal: upstream.signal,
    });
    tally.requests += 1;
    return { presented, answer, fresh };
  };

  let sent: Sent;
  try {
    sent = await send();
    // A token the broker no longer holds live (R3): that token alone is dropped, and the request
    // is sent once more with the token the next trade gives. A JSON 401's body is read here, from
    // a clone, before `giveBack` writes the head. Where that body breaks off, this read reaches the
    // break-off first, and on Bun 1.4.4 the body `giveBack` streams then yields none of the bytes
    // Dispatch sent: the tool reads the 401's status line and headers, no body byte, then the
    // close, still a cut answer.
    if (await tokenNotLive(sent.answer)) {
      run.config.credential.stale?.(undefined, sent.presented);
      await sent.answer.body?.cancel();
      sent = await send();
      if (sent.fresh) tally.renewed += 1;
    }
  } catch (error) {
    if (upstream.signal.aborted) return;
    if (error instanceof TradeRefused) {
      answerRefusal(response, TRADE_REFUSED, error.message);
      return;
    }
    answerRefusal(
      response,
      DISPATCH_UNREACHABLE,
      `Dispatch at ${run.config.url} did not answer: ${messageFor(error)}`
    );
    return;
  }
  await giveBack(sent.answer, response);
}

/**
 * Dispatch's answer to the tool as Dispatch sent it (R3): its status, its end-to-end headers and
 * its body, streamed as it arrives with no total deadline, since some routes stream
 * (`/api/v1/going/requests/{id}/stream`). {@link answeredHeaders} gives back Dispatch's
 * `Content-Length` whatever the `Content-Encoding`, and leaves it out only where the answer also
 * carries `Transfer-Encoding`: a body with that length goes out unchunked, and any other body goes
 * out chunked. A redirect is given back, never followed.
 *
 * It writes the status line and the headers, flushes them, streams the body as it comes, and ends
 * the answer. A body that breaks off throws to {@link proxyServer}'s catch, which closes the
 * connection. Where the tool hangs up, `serve` stops the fetch, and the loop below ends by the same
 * throw or at the next chunk.
 */
async function giveBack(answer: Response, response: ServerResponse): Promise<void> {
  response.writeHead(answer.status, answeredHeaders(answer.headers));
  if (answer.body === null) {
    response.end();
    return;
  }
  // Bun's `node:http` puts nothing on the wire until the first write. On a response that has its
  // socket, this queues the status line and the headers, so a body that breaks off before its
  // first byte still has them sent. A response queued behind an earlier answer on a pipelined
  // connection has no socket yet, and this queues nothing for it.
  response.flushHeaders();
  for await (const chunk of answer.body) {
    if (response.destroyed) return;
    if (!response.write(chunk)) await drained(response);
  }
  response.end();
}

/** Resolves once `response` takes more, or once it is closed. */
function drained(response: ServerResponse): Promise<void> {
  const { promise, resolve } = Promise.withResolvers<void>();
  const done = (): void => {
    response.off("drain", done);
    response.off("close", done);
    resolve();
  };
  response.on("drain", done);
  response.on("close", done);
  return promise;
}
