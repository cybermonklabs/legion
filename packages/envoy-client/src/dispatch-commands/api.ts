// packages/envoy-client/src/dispatch-commands/api.ts
//
// `dispatch api <METHOD> <path> [--body-file <file>|-]`: sends one request of Dispatch's HTTP API
// with the credential every `dispatch` command presents (a set bearer, the token files' broker
// token, else a token traded at the secrets broker, with a new DPoP proof for the request), and
// prints the answer's body. It is for a script and a job: a broker token is bound to a key, so no
// script can send it as a bearer, and a GitHub Actions run, which no host plugin runs, needs only
// DISPATCH_URL and the broker's variables. It is no Dispatch tool of `@legion/contracts`' specs,
// since no agent tool needs it.
//
// It takes only `/api/v1` and the paths under `/api/v1/` of the Dispatch it is set for, spelled as
// the URL parser reads them, so the credential goes to no other host and no other route. It sends
// the request once: no retry, no redirect followed, no `Idempotency-Key`. In a session whose
// AskGate reviews Dispatch writes it sends GET alone, and names the command that makes the write,
// which the gate reviews (`gatedWrite`, src/askgate-marker.ts).

import { errorClass, urlForText } from "@legion/contracts/log-text";
import { type AskgateState, askgateState } from "../askgate-marker";
import { credentialDeadline } from "../bearer";
import { brokerRouteConfigured } from "../broker-credential";
import type { DispatchCliIo } from "../dispatch-cli";
import {
  type ActiveDispatchConfig,
  type DispatchEnvironment,
  dispatchSettings,
  tokenFilesSet,
} from "../dispatch-config";
import { DISPATCH_TOOL_DEADLINE_MS, dispatchRefusal } from "../dispatch-http";
import { sessionDirectory } from "../dispatch-session-directory";
import type { ResultInput } from "../dispatch-session-state";
import type { EnvoyConfig } from "../envoy-config";
import { messageFor } from "../errors";
import { OK, REFUSED, USAGE } from "./exit-codes";
import { type HostSession, hostedDispatch, SESSION_VARIABLES } from "./host-session";

/** The methods it sends. */
const METHODS = ["GET", "POST", "PUT", "PATCH", "DELETE"] as const;
type Method = (typeof METHODS)[number];

/** The one route it takes whole, which lists every route; it takes each path under it too. */
const API_ROOT = "/api/v1";

/** Why the gate refuses a write here, as the refusal and its ledger line's AskGate mark say it:
 *  AskGate reviews the `dispatch` commands, and no `dispatch api` call. */
const GATED_WHY =
  "this session's AskGate reviews Dispatch writes, so dispatch api sends only GET here";

/** `dispatch api --help`. */
const API_HELP = [
  "Usage: dispatch api <METHOD> <path> [--body-file <file>|-]",
  "",
  "Sends one request of Dispatch's HTTP API with the credential every dispatch command presents, and prints the answer's body. <METHOD> is GET, POST, PUT, PATCH or DELETE. <path> is /api/v1, which lists every route, or a path under /api/v1/, with its query if it has one, spelled as the URL parser reads it (no `.`, `..`, `%2e` or `\\`); any other path, and a URL, is refused before anything is sent, so the credential goes to no other server.",
  "",
  "Flags:",
  "  --body-file <file>|-",
  "      Sends the file's text, or standard input with -, as the request's JSON body (Content-Type: application/json). GET takes no body. Standard input is read only with this flag.",
  "  --help",
  "      Prints this help.",
  "",
  "The answer's body goes to standard output; every line of the command's own goes to standard error. It exits 0 on a 2xx answer; 1 on any other answer, naming its status and Dispatch's code, on no answer, on a credential that could not be presented, and on a write this session's AskGate refuses; and 2 on a usage error, which sends nothing. It sends the request once: it follows no redirect and sends no Idempotency-Key.",
  "",
  "Under an agent's host it acts as that host's session, as every dispatch command does. With no host, as in a GitHub Actions job, it reads the environment alone: DISPATCH_URL, and DISPATCH_TOKEN, DISPATCH_TOKEN_FILE, a broker token traded elsewhere in DISPATCH_BROKER_TOKEN_FILE and DISPATCH_BROKER_DPOP_KEY_FILE, or the secrets broker's AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY, whose trade names DISPATCH_SESSION_ID, else AGENT_SECRETS_SESSION.",
  "",
  "In a session whose AskGate reviews Dispatch writes it sends only GET: make a write with the dispatch command that makes it, which AskGate reviews.",
  "",
  "Example: dispatch api GET /api/v1/whoami",
  "",
].join("\n");

/** Where a route no command writes leaves the command its refusal names. */
const NO_ROW = "the dispatch command that makes it (dispatch --help lists them)";

/** What the command refuses with, where it has no host, when the environment names no Dispatch
 *  or no credential for it. */
const NO_DISPATCH =
  "no Dispatch is set; set DISPATCH_URL, and a credential (DISPATCH_TOKEN, DISPATCH_TOKEN_FILE, DISPATCH_BROKER_TOKEN_FILE and DISPATCH_BROKER_DPOP_KEY_FILE, or the broker's AGENT_SECRETS_URL and AGENT_SECRETS_MACHINE_IDENTITY)";

/** The configuration a command with no host reads: no envoy.json at all, so no file under HOME
 *  speaks for a job that no session runs, and Dispatch is read from the environment alone. */
const NO_FILE: EnvoyConfig = {
  kind: "read",
  path: "",
  dispatch: {},
  envoy: {},
  checkout: null,
  missing: null,
  warnings: [],
};

/** A write route a `dispatch` command sends: its method, its path with each `{name}` standing for
 *  one segment, and the commands that send it. */
export interface WriteRoute {
  readonly method: Exclude<Method, "GET">;
  readonly pattern: string;
  readonly commands: readonly string[];
}

/**
 * Each write route a `dispatch` command sends, with the commands that send it, which the refusal
 * of a gated session names. A route that writes nothing (`markdown/round-trip`, `related`,
 * `results/dry-run`) names the command that sends it, since the gate refuses it all the same. A
 * write method of `DispatchClient` that no command calls has no row: `dispatch-api-routes.test.ts`
 * names each in its `NO_COMMAND`, and holds the table to every write `DispatchClient` sends.
 */
export const WRITE_ROUTES: readonly WriteRoute[] = [
  { method: "POST", pattern: "/api/v1/issues", commands: ["issue"] },
  { method: "PATCH", pattern: "/api/v1/issues/{key}", commands: ["issue-update"] },
  { method: "POST", pattern: "/api/v1/issues/{key}/status-change", commands: ["issue-update"] },
  { method: "POST", pattern: "/api/v1/issues/{key}/claim", commands: ["claim"] },
  { method: "DELETE", pattern: "/api/v1/issues/{key}/claim", commands: ["claim"] },
  { method: "POST", pattern: "/api/v1/issues/{key}/asks", commands: ["ask"] },
  { method: "POST", pattern: "/api/v1/artifacts/{id}/asks", commands: ["ask"] },
  { method: "PATCH", pattern: "/api/v1/asks/{id}", commands: ["edit-ask"] },
  { method: "POST", pattern: "/api/v1/asks/{id}/resolve", commands: ["resolve-ask"] },
  { method: "PUT", pattern: "/api/v1/asks/{id}/followers/{session}", commands: ["follow"] },
  { method: "DELETE", pattern: "/api/v1/asks/{id}/followers/{session}", commands: ["follow"] },
  { method: "POST", pattern: "/api/v1/issues/{key}/comments", commands: ["comment", "suggest"] },
  { method: "POST", pattern: "/api/v1/artifacts/{id}/comments", commands: ["comment", "suggest"] },
  { method: "POST", pattern: "/api/v1/comments/{id}/resolve", commands: ["resolve-comment"] },
  // The author's corrections of its own comment and issue message.
  { method: "PATCH", pattern: "/api/v1/comments/{id}", commands: ["edit-comment"] },
  { method: "DELETE", pattern: "/api/v1/comments/{id}", commands: ["delete-comment"] },
  { method: "PATCH", pattern: "/api/v1/messages/{id}", commands: ["edit-message"] },
  { method: "DELETE", pattern: "/api/v1/messages/{id}", commands: ["delete-message"] },
  // issue-update posts a closing reason there (`issues.ts`).
  {
    method: "POST",
    pattern: "/api/v1/issues/{key}/messages",
    commands: ["message", "issue-update"],
  },
  // A reply, and the reply's pictures.
  { method: "POST", pattern: "/api/v1/messages/{id}/reply", commands: ["message"] },
  { method: "POST", pattern: "/api/v1/agents/{session}/artifacts", commands: ["message"] },
  { method: "POST", pattern: "/api/v1/agents/{session}/messages", commands: ["send-to-agent"] },
  { method: "POST", pattern: "/api/v1/artifacts/{id}/edits", commands: ["doc-edit"] },
  {
    method: "POST",
    pattern: "/api/v1/artifacts/{id}/approval-requests",
    commands: ["request-approval"],
  },
  { method: "POST", pattern: "/api/v1/artifacts/{id}/plan-reviews", commands: ["plan-approve"] },
  {
    method: "POST",
    pattern: "/api/v1/artifacts/{id}/versions/{number}/reviews",
    commands: ["review"],
  },
  { method: "POST", pattern: "/api/v1/issues/{key}/results", commands: ["result"] },
  { method: "POST", pattern: "/api/v1/issues/{key}/results/dry-run", commands: ["result"] },
  { method: "POST", pattern: "/api/v1/results/{id}/retract", commands: ["result"] },
  { method: "POST", pattern: "/api/v1/issues/{key}/handoffs/{phase}", commands: ["handoff-write"] },
  // The first two also carry the pictures of a comment, an ask or a message (`postWithPictures`,
  // command-context.ts).
  { method: "POST", pattern: "/api/v1/issues/{key}/artifacts", commands: ["artifact"] },
  { method: "POST", pattern: "/api/v1/projects/{key}/artifacts", commands: ["artifact"] },
  { method: "POST", pattern: "/api/v1/markdown/round-trip", commands: ["artifact"] },
  { method: "POST", pattern: "/api/v1/projects", commands: ["project"] },
  { method: "POST", pattern: "/api/v1/projects/{key}/related", commands: ["related"] },
  {
    method: "POST",
    pattern: "/api/v1/projects/{key}/architecture-sources/sync",
    commands: ["architecture-sync"],
  },
  {
    method: "POST",
    pattern: "/api/v1/projects/{key}/architecture-source/sync",
    commands: ["architecture-sync"],
  },
  {
    method: "POST",
    pattern: "/api/v1/projects/{key}/architecture-sources/{owner}/{repo}/sync",
    commands: ["architecture-sync"],
  },
];

/** The row of {@link WRITE_ROUTES} that `method` on `path` (a URL's pathname) matches, or
 *  undefined: each segment of the row's pattern as written, and each `{name}` one segment that is
 *  not empty. */
export function writeRoute(method: string, path: string): WriteRoute | undefined {
  const segments = path.split("/");
  return WRITE_ROUTES.find((row) => {
    const pattern = row.pattern.split("/");
    return (
      row.method === method &&
      pattern.length === segments.length &&
      pattern.every((part, at) =>
        part.startsWith("{") ? segments[at] !== "" : part === segments[at]
      )
    );
  });
}

/** The arguments `argv` gives, or the problem with them. */
type ParsedApi =
  | { readonly kind: "help" }
  | {
      readonly kind: "call";
      readonly method: Method;
      readonly path: string;
      readonly bodyFile: string | undefined;
    }
  | { readonly kind: "refused"; readonly problem: string };

function parseApi(argv: readonly string[]): ParsedApi {
  const words: string[] = [];
  let bodyFile: string | undefined;
  for (let at = 0; at < argv.length; at += 1) {
    const word = argv[at] as string;
    if (word === "--help" || word === "-h") return { kind: "help" };
    if (word === "--body-file" || word.startsWith("--body-file=")) {
      if (bodyFile !== undefined) return { kind: "refused", problem: "--body-file is given twice" };
      const operand = word === "--body-file" ? argv[++at] : word.slice("--body-file=".length);
      if (operand === undefined || operand === "") {
        return {
          kind: "refused",
          problem: "--body-file needs a file, or - for standard input",
        };
      }
      bodyFile = operand;
    } else if (word.startsWith("--")) {
      return { kind: "refused", problem: `unknown flag ${JSON.stringify(word)}` };
    } else {
      words.push(word);
    }
  }
  const [method, path, extra] = words;
  if (method === undefined || path === undefined) {
    return {
      kind: "refused",
      problem: "it takes a method and a path, as in dispatch api GET /api/v1/whoami",
    };
  }
  if (extra !== undefined) {
    return { kind: "refused", problem: `unknown argument ${JSON.stringify(extra)}` };
  }
  const known = METHODS.find((name) => name === method);
  if (known === undefined) {
    return {
      kind: "refused",
      problem: `the method ${JSON.stringify(method)} is none of ${METHODS.join(", ")}`,
    };
  }
  if (!path.startsWith("/") || path.startsWith("//")) {
    return {
      kind: "refused",
      problem:
        "the path must start with a single /: it takes /api/v1 and the paths under /api/v1/ of the Dispatch it is set for, never a URL, so the credential goes to no other server",
    };
  }
  if (known === "GET" && bodyFile !== undefined) {
    return { kind: "refused", problem: "GET takes no body, so --body-file is refused with it" };
  }
  return { kind: "call", method: known, path, bodyFile };
}

/** Whether the environment names no Dispatch credential: no bearer variable, and no broker and
 *  identity to trade. A broker setting that cannot be read names one, whose problem is said. */
function namesNoCredential(env: DispatchEnvironment): boolean {
  if (env.DISPATCH_TOKEN !== undefined || env.DISPATCH_TOKEN_FILE !== undefined) return false;
  try {
    return !brokerRouteConfigured(env).traded;
  } catch {
    return false;
  }
}

/** The answer's `code` where its body is Dispatch's own JSON refusal naming one, by the rule
 *  `DispatchClient` reads one by (`dispatchRefusal`, dispatch-http.ts). Any other body, and a code
 *  that is no word of Dispatch's codes, is `with no code`. */
function codeOf(answer: Response, text: string): string {
  const code = dispatchRefusal(answer, text)?.code;
  return code !== undefined && /^[A-Z][A-Z0-9_]*$/.test(code) ? code : "with no code";
}

/**
 * Runs `dispatch api` with `argv` (the words after `api`) in `env`, and answers its exit code: 0 on
 * a 2xx answer, 1 on any other answer, on no answer, on a credential that could not be presented
 * and on a write this session's AskGate refuses, and 2 on a usage error, which sends nothing.
 * `hosted` is the host `commandHost` found and the session the command acts as under it
 * (`hostSession`), or undefined where it found none: the command then reads Dispatch from the
 * environment alone. The answer's body goes to `io.stdout`, every line of its own to `io.stderr`.
 *
 * Each refusal made before the request is sent, a usage error or not, also goes to `ledger`, the
 * session's ledger as `runDispatchCli` writes every refusal before a call reaches Dispatch, an
 * AskGate refusal with AskGate's mark (`askgate`), as the CLI's own refusal of a gated write
 * carries it, so pi-envoy counts it. A request that was sent records nothing there, whatever
 * Dispatch answered: a call's outcome is recorded for the agent tools the tool specs describe, and
 * `dispatch api` is none of them.
 */
export async function apiCommand(
  argv: readonly string[],
  env: Readonly<Record<string, string>>,
  io: Pick<DispatchCliIo, "stdout" | "stderr" | "readText" | "cwd" | "fetchImpl">,
  options: {
    readonly hosted: HostSession | undefined;
    readonly ledger: (refusal: string, askgate?: ResultInput["askgate"]) => void;
  }
): Promise<number> {
  const { hosted, ledger } = options;
  const say = (text: string): void => io.stderr(`${text}\n`);
  /** Prints `text`, a refusal before anything is sent, and puts it in the session's ledger. */
  const stop = (text: string, code: number, askgate?: ResultInput["askgate"]): number => {
    say(text);
    ledger(text, askgate);
    return code;
  };
  const refuse = (problem: string, code: number, askgate?: ResultInput["askgate"]): number =>
    stop(`dispatch api was not called: ${problem}`, code, askgate);
  const parsed = parseApi(argv);
  if (parsed.kind === "help") {
    io.stdout(API_HELP);
    return OK;
  }
  if (parsed.kind === "refused")
    return refuse(`${parsed.problem}. Run dispatch api --help.`, USAGE);
  const { method, path, bodyFile } = parsed;

  // The directory of every session the environment names, host or none: an Oh My Pi session's
  // shell with DISPATCH_HOST emptied still names its session. An id that names no directory fails
  // closed, a GET too, naming the variable to unset rather than the value it holds. Each one's
  // AskGate record is read for a write alone (below), whatever host the command finds: every
  // session the refusal ledger can be written for (`SESSION_VARIABLES`).
  const sessions: string[] = [];
  for (const variable of SESSION_VARIABLES) {
    const id = env[variable];
    if (id === undefined) continue;
    try {
      sessions.push(sessionDirectory(env, id));
    } catch {
      return refuse(
        `${variable} holds no session id whose AskGate record can be read: an id is 1-128 letters, digits, ".", "_" or "-". Unset ${variable}, or set it to the id of your own session.`,
        USAGE
      );
    }
  }

  let config: ActiveDispatchConfig;
  if (hosted === undefined) {
    const resolution = await dispatchSettings(env, NO_FILE, {
      session: env.DISPATCH_SESSION_ID,
      tokenFiles: true,
    });
    if (!resolution.enabled) {
      // The off texts of an unset Dispatch or credential name an envoy.json, which a command with
      // no host does not read; any other says what was wrong with what is set, the token files'
      // refusal among them, which names DISPATCH_URL where it is unset beside them.
      return tokenFilesSet(env) === undefined &&
        (env.DISPATCH_URL === undefined || namesNoCredential(env))
        ? refuse(NO_DISPATCH, USAGE)
        : refuse(`dispatch config: ${resolution.error ?? resolution.notice}`, USAGE);
    }
    config = resolution;
  } else {
    if ("problem" in hosted) return stop(hosted.problem, USAGE);
    const resolved = await hostedDispatch(hosted.file, env, { cwd: io.cwd, session: hosted.sessionId });
    for (const line of resolved.warnings) say(line);
    if ("problem" in resolved) return stop(resolved.problem, USAGE);
    config = resolved.config;
  }

  // The path as the URL parser reads it against this Dispatch: its origin, under /api/v1, and
  // spelled as the parser keeps it, so a `..`, a `%2e%2e` or a `\` the parser would turn into
  // another route is refused rather than sent. The query goes as written: the path refusal below
  // names the route without it, and each line that names the request (`request`) names it too.
  const query = path.indexOf("?");
  const route = query < 0 ? path : path.slice(0, query);
  let target: URL | undefined;
  try {
    target = new URL(path, `${config.url}/`);
  } catch {
    // No URL at all: refused below with every other path it does not take.
  }
  if (
    target === undefined ||
    target.origin !== new URL(config.url).origin ||
    (target.pathname !== API_ROOT && !target.pathname.startsWith(`${API_ROOT}/`)) ||
    target.pathname !== route
  ) {
    return refuse(
      `it takes /api/v1 and the paths under /api/v1/ of the Dispatch it is set for, spelled as the URL parser reads them (no ".", "..", "%2e" or "\\"), and ${JSON.stringify(route)} is none of them. Run dispatch api --help.`,
      USAGE
    );
  }

  if (method !== "GET") {
    const gates = sessions.map((dir) => askgateState(dir));
    const broken = gates.find(
      (gate): gate is Extract<AskgateState, { readonly kind: "broken" }> => gate.kind === "broken"
    );
    const gated = gates.some((gate) => gate.kind === "on");
    if (gated || broken !== undefined) {
      const row = writeRoute(method, target.pathname);
      const command =
        row === undefined ? NO_ROW : row.commands.map((name) => `dispatch ${name}`).join(" or ");
      return gated || broken === undefined
        ? refuse(`${GATED_WHY}; make this write with ${command}`, REFUSED, {
            mode: "block",
            why: GATED_WHY,
          })
        : refuse(
            `AskGate's record for this session, ${broken.problem}, so whether AskGate reviews this session's Dispatch writes cannot be told, and dispatch api sends only GET here; make this write with ${command}`,
            REFUSED,
            { mode: "block", why: broken.problem }
          );
    }
  }

  let body: string | undefined;
  if (bodyFile !== undefined) {
    try {
      body = io.readText(bodyFile);
    } catch (error) {
      return refuse(
        `--body-file could not read ${bodyFile === "-" ? "standard input" : bodyFile}: ${messageFor(error)}.`,
        USAGE
      );
    }
  }

  const request = `${method} ${path}`;
  const deadline = AbortSignal.timeout(DISPATCH_TOOL_DEADLINE_MS);
  let presented: Readonly<Record<string, string>>;
  try {
    presented = await config.credential.headers(
      method,
      target.href,
      undefined,
      credentialDeadline(deadline, DISPATCH_TOOL_DEADLINE_MS)
    );
  } catch (error) {
    // The credential could not be presented, so nothing reached Dispatch.
    return stop(`dispatch api: ${request} was not sent: ${messageFor(error)}`, REFUSED);
  }
  let answer: Response;
  let text: string;
  try {
    answer = await (io.fetchImpl ?? fetch)(target.href, {
      method,
      headers: {
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
        ...presented,
      },
      ...(body === undefined ? {} : { body }),
      // A 3xx is no answer of Dispatch's API routes, and a followed one would carry the request,
      // and its credential, wherever it points.
      redirect: "manual",
      signal: deadline,
    });
    text = await answer.text();
  } catch (error) {
    say(
      `dispatch api: ${request} got no answer from Dispatch at ${urlForText(config.url, "the configured Dispatch URL")}: ${errorClass(error)}`
    );
    return REFUSED;
  }
  io.stdout(text);
  if (answer.ok) return OK;
  say(`dispatch api: ${request} answered ${answer.status} ${codeOf(answer, text)}`);
  return REFUSED;
}
