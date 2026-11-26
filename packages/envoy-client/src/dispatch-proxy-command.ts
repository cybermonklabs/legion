import { resolve } from "node:path";

/**
 * The surface of `dispatch proxy --socket <path> -- <command>`, a command of the `dispatch`
 * command line that no Dispatch tool spec defines: its name, the sentence `dispatch --help` lists,
 * its help, its exit line and the parse of its own arguments. The proxy itself is
 * `dispatch-proxy.ts`.
 *
 * This module imports nothing that imports `@legion/cipher-client`: the docs generator's program
 * is `dispatch-command.ts`'s, which imports this module and holds no cipher-client source
 * (`.github/ci-gates/docs.py`, the docs gate's read of envoy-client's sources).
 */

/** The proxy's command word. */
export const PROXY_COMMAND = "proxy";

/** What `dispatch --help` lists for the proxy. */
export const PROXY_SENTENCE =
  "Runs a command with a proxy on a unix socket that sends the command's Dispatch reads, and its comment POSTs that carry an Idempotency-Key, with this session's broker token.";

/** The longest path a unix socket address holds (`sun_path` less its terminating NUL). */
export const SOCKET_PATH_MAX_BYTES = 107;

/**
 * The line the proxy ends every run with on standard error, a start failure included, in its fixed
 * form: `requests` sent to Dispatch, the one resend after `BROKER_TOKEN_NOT_ACTIVE` counted;
 * `trades` that gave a token, the first one included; and `renewed`, those of them that followed
 * a `BROKER_TOKEN_NOT_ACTIVE` answer. The plural is fixed, so a reader matches one form.
 */
export function proxyExitLine(requests: number, trades: number, renewed: number): string {
  return `dispatch proxy: ${requests} requests, ${trades} trades, ${renewed} after BROKER_TOKEN_NOT_ACTIVE`;
}

/** What `dispatch proxy --help` prints. */
export function proxyHelp(): string {
  return [
    "Usage: dispatch proxy --socket <path> [--dry-run] -- <command> [<argument>...]",
    "",
    PROXY_SENTENCE,
    "",
    "The proxy listens on a unix socket at <path> for the whole run of <command>. It sends each",
    "request it takes to the Dispatch server and the session that a dispatch command resolves in",
    "the same environment, with this session's broker token and a new DPoP proof. So the tool",
    "holds no credential and signs nothing. It adds the path of each request to the path of the",
    "Dispatch URL, and it does not decode the query. The proxy trades for the token once before it",
    "starts the command, and again only when the token has less than 5 minutes to live, or after",
    "Dispatch answers 401 BROKER_TOKEN_NOT_ACTIVE. It runs only under DISPATCH_TOKEN_BROKER=1.",
    "",
    "What it sends:",
    "  GET /api/...                         any GET whose path starts with /api/",
    "  POST /api/v1/issues/<key>/comments   a comment that carries an Idempotency-Key header",
    "It sends no other write. A tool reads much and writes little. Where a dispatch command sends a",
    "write, the write goes through that command, so AskGate reviews it wherever AskGate gates that",
    "command. The proxy's refusal of the write names that command. Where no other dispatch command",
    "sends a write under /api/v1, the refusal names dispatch api --method, which sends it outside a",
    "session whose AskGate reviews Dispatch writes. The comment needs a key so that a resend posts",
    "it once. The comment POST passes no AskGate review. The proxy is not one of the writes that",
    "AskGate gates, and the Dispatch server checks no marker. A session that holds a Dispatch token",
    "can post the same comment with that token already.",
    "",
    "It listens on a unix socket only, made with mode 0600, and opens no TCP port. The roles of a",
    "pod share the pod's network, so a port would let another role act as this one. A socket path",
    "that exists is refused, so give it a path in a new directory (mktemp -d). A socket that a",
    "killed proxy left behind refuses the next start.",
    "",
    "How a tool calls it: the command runs with DISPATCH_PROXY_SOCKET set to the socket's path.",
    "The host in a URL is not read. Write the path and the query.",
    '  curl --unix-socket "$DISPATCH_PROXY_SOCKET" http://dispatch/api/v1/issues',
    "  In Python, an http.client.HTTPConnection whose connect opens an AF_UNIX socket:",
    "    import http.client, os, socket",
    "    class ProxyConnection(http.client.HTTPConnection):",
    "        def connect(self):",
    "            self.sock = socket.socket(socket.AF_UNIX, socket.SOCK_STREAM)",
    '            self.sock.connect(os.environ["DISPATCH_PROXY_SOCKET"])',
    '    connection = ProxyConnection("dispatch")',
    '    connection.request("GET", "/api/v1/issues?project=DSP")',
    "    answer = connection.getresponse()",
    "    print(answer.status, answer.read())",
    "",
    "What it answers: Dispatch's status, headers and body as Dispatch sent them. An answer that",
    "carries Transfer-Encoding as well as Content-Length is the one exception: it comes back",
    "without that Content-Length (RFC 9112, section 6.3), with its body chunked. It follows no",
    "redirect. Its own refusals have a JSON body with a code and an error, and each code starts",
    "with PROXY_. No code of Dispatch's does. It never answers 403 or 404 of its own.",
    "  400 PROXY_TARGET_REFUSED        a target that names a host (http://... or //...)",
    "  405 PROXY_METHOD_REFUSED        each other method or path, and a comment POST with no",
    "                                  Idempotency-Key. The error names the dispatch command to",
    "                                  use, where one exists. For a write under /api/v1 that no",
    "                                  other dispatch command sends, it names dispatch api",
    "                                  --method. It also refuses a target that holds a # or a",
    "                                  control character, and a path with a . or .. segment",
    "                                  (plain or percent-encoded), a backslash, or %2F or %5C in",
    "                                  either case: each can reach another route than the one the",
    "                                  target names.",
    "  502 PROXY_TRADE_REFUSED         the token has ended and the broker refused a new one. The",
    "                                  error is in the broker's words.",
    "  502 PROXY_DISPATCH_UNREACHABLE  Dispatch did not answer",
    "",
    "Flags:",
    "  --socket <path>",
    "      The socket to listen on. Also --socket=<path>. A path over 107 bytes is refused.",
    "  --dry-run",
    "      Checks the arguments and the environment, prints the Dispatch server, the session and",
    "      the socket it would use, and starts nothing.",
    "  --help",
    "      Prints this help.",
    "Every word after -- is the command's, flags included.",
    "",
    "It exits with the command's status. A command that a signal ended is 128 plus the signal's",
    "number. A command that does not exist is 127, and one that cannot run is 126. It exits 2 when",
    "it cannot start, and runs no command then. It sends SIGTERM, SIGINT and SIGHUP on to the",
    "command and serves until the command ends. Then it removes its socket. The last line it",
    "writes on standard error is:",
    "  dispatch proxy: <n> requests, <m> trades, <r> after BROKER_TOKEN_NOT_ACTIVE",
    "Here n counts the requests sent to Dispatch, m the trades that gave a token, and r those of",
    "them that followed a BROKER_TOKEN_NOT_ACTIVE answer.",
    "",
    'Example: dispatch proxy --socket "$(mktemp -d)/d.sock" -- python3 work-cost.py',
    "",
  ].join("\n");
}

/** `dispatch proxy`'s arguments, as `parseProxyArgv` reads them. */
export type ParsedProxyArgv =
  | { readonly kind: "help" }
  | { readonly kind: "refused"; readonly problem: string }
  | {
      readonly kind: "run";
      /** The socket's path, made absolute against the working directory, so the command finds
       *  it in `DISPATCH_PROXY_SOCKET` wherever it changes directory to. */
      readonly socket: string;
      readonly dryRun: boolean;
      /** The command and its arguments: every word after `--`. */
      readonly command: readonly string[];
    };

/**
 * Parses the words after `proxy`: `--socket <path>` or `--socket=<path>` once, `--dry-run`,
 * `--help`, then `--` and at least one word, which is the command. Any other word before `--`, a
 * second `--socket`, a path over {@link SOCKET_PATH_MAX_BYTES} bytes once resolved against `cwd`,
 * and no command are refused. `--help` before `--` prints the help whatever else is given.
 */
export function parseProxyArgv(rest: readonly string[], cwd: string): ParsedProxyArgv {
  const separator = rest.indexOf("--");
  const own = separator === -1 ? rest : rest.slice(0, separator);
  if (own.includes("--help")) return { kind: "help" };
  let socket: string | undefined;
  let dryRun = false;
  for (let at = 0; at < own.length; at += 1) {
    const word = own[at] ?? "";
    if (word === "--dry-run") {
      dryRun = true;
      continue;
    }
    if (word !== "--socket" && !word.startsWith("--socket=")) {
      return {
        kind: "refused",
        problem:
          separator === -1
            ? `${JSON.stringify(word)} is no flag of dispatch proxy, and the command goes after --.`
            : `${JSON.stringify(word)} is no flag of dispatch proxy.`,
      };
    }
    if (socket !== undefined) return { kind: "refused", problem: "--socket is given twice." };
    let value: string | undefined;
    if (word === "--socket") {
      at += 1;
      value = own[at];
    } else {
      value = word.slice("--socket=".length);
    }
    if (value === undefined || value === "" || value.startsWith("-")) {
      return { kind: "refused", problem: "--socket takes the path of the socket to listen on." };
    }
    socket = value;
  }
  if (socket === undefined) {
    return { kind: "refused", problem: "--socket <path> is missing." };
  }
  const command = separator === -1 ? [] : rest.slice(separator + 1);
  if (command.length === 0) {
    return {
      kind: "refused",
      problem: "No command is given: write it after --, as -- <command> [<argument>...].",
    };
  }
  const path = resolve(cwd, socket);
  const bytes = Buffer.byteLength(path);
  if (bytes > SOCKET_PATH_MAX_BYTES) {
    return {
      kind: "refused",
      problem: `The socket path ${JSON.stringify(path)} is ${bytes} bytes, over the ${SOCKET_PATH_MAX_BYTES} a unix socket path holds. Give a shorter path, such as one in a new directory of mktemp -d.`,
    };
  }
  return { kind: "run", socket: path, dryRun, command };
}
