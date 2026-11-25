// packages/envoy-client/src/dispatch-commands/api.ts
//
// `dispatch api` (`dispatch_api` in `@legion/contracts`): one request of a route under `/api/v1`
// that no named command sends, a GET or the method `--method` names, sent through the client
// every command uses, so it presents the credential every command presents (a set bearer, the
// token files' broker token, else a token traded at the secrets broker, with a new DPoP proof for
// the request), and Dispatch's JSON answer printed for `jq` to read. The contract's rule refused
// a path that could leave `/api/v1` (`apiPathProblem`), and a body with no method
// (`apiCallProblem`), before this runs. The request goes once (`DispatchClient.readRoute` and
// `sendRoute`), and once more only after Dispatch answers that the token it presented is not
// live (Dispatch authenticated nothing, so nothing ran), with no redirect followed and no
// `Idempotency-Key`. A refusal is Dispatch's own error, thrown as the client throws it, which the
// command prints and exits 1 on.
//
// A script and a job send it too: a broker token is bound to a key, so no script can send it as a
// bearer. Where no host is found, as in a GitHub Actions job, the CLI reads Dispatch from the
// environment alone (`unhostedDispatch`). In a session whose AskGate reviews Dispatch writes it
// sends GET alone, and names the command that makes the write, which the gate reviews
// (`gatedWrite`, src/askgate-marker.ts). The gate sits in the command itself (`sendApiCall`),
// under a host and with none.

import type { DispatchApiMethod } from "@legion/contracts";
import { type AskgateState, askgateState } from "../askgate-marker";
import { brokerRouteConfigured } from "../broker-credential";
import {
  type ActiveDispatchConfig,
  type DispatchEnvironment,
  dispatchSettings,
  tokenFilesSet,
} from "../dispatch-config";
import type { DispatchClient } from "../dispatch-http";
import { sessionDirectory } from "../dispatch-session-directory";
import type { EnvoyConfig } from "../envoy-config";
import { ToolInputError } from "../tool-input-errors";
import {
  type DispatchToolResult,
  defined,
  optionalString,
  type SessionContext,
  stringArg,
  type ToolArguments,
} from "./context";
import { SESSION_VARIABLES } from "./host-session";

/** Why the gate refuses a write here, as the refusal and its ledger line's AskGate mark say it:
 *  AskGate reviews the `dispatch` commands, and no `dispatch api` call. */
const GATED_WHY =
  "this session's AskGate reviews Dispatch writes, so dispatch api sends only GET here";

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
  readonly method: DispatchApiMethod;
  readonly pattern: string;
  readonly commands: readonly string[];
}

/**
 * Each write route a `dispatch` command sends, with the commands that send it, which the refusal
 * of a gated session names. A route that writes nothing (`markdown/round-trip`, `related`,
 * `results/dry-run`) names the command that sends it, since the gate refuses it all the same. A
 * write method of `DispatchClient` that no command calls has no row, and neither has `sendRoute`,
 * which sends the route its caller names: `dispatch-api-routes.test.ts` names each in its
 * `NO_COMMAND`, and holds the table to every write `DispatchClient` sends.
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

/** The row of {@link WRITE_ROUTES} that `method` on `path` (a route, without its query) matches,
 *  or undefined: each segment of the row's pattern as written, and each `{name}` one segment that
 *  is not empty. */
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

/**
 * A write `dispatch api` refused before anything was sent, in a session whose AskGate record is
 * live or cannot be read: it exits 1, and its ledger line carries AskGate's mark (`askgate`), as
 * the CLI's own refusal of a gated write carries it, so pi-envoy counts it.
 */
export class ApiGateRefusal extends Error {
  override readonly name = "ApiGateRefusal";

  constructor(
    message: string,
    readonly askgate: { readonly mode: "block"; readonly why: string }
  ) {
    super(message);
  }
}

/**
 * The refusal of `method` on `path` in `env`, or undefined where AskGate reviews no Dispatch write
 * of any session `env` names. Every session id of `SESSION_VARIABLES` that is set is read, host or
 * none: an Oh My Pi session's shell with DISPATCH_HOST emptied still names its session, and every
 * session the refusal ledger can be written for is one whose gate is read. A record that cannot be
 * read counts as live. An id that names no directory refuses the write too, as a usage error that
 * names the variable to unset rather than the value it holds. It returns rather than throws, so
 * the throw stays at the call.
 */
function gatedWriteRefusal(
  method: DispatchApiMethod,
  path: string,
  env: Readonly<Record<string, string | undefined>>
): Error | undefined {
  const gates: AskgateState[] = [];
  for (const variable of SESSION_VARIABLES) {
    const id = env[variable];
    if (id === undefined || id === "") continue;
    let dir: string;
    try {
      dir = sessionDirectory(env, id);
    } catch {
      return new ToolInputError(
        "dispatch_api",
        [
          `${variable} holds no session id whose AskGate record can be read: an id is 1-128 letters, digits, ".", "_" or "-". Unset ${variable}, or set it to the id of your own session.`,
        ],
        { aboutAFlag: false }
      );
    }
    gates.push(askgateState(dir));
  }
  const broken = gates.find(
    (gate): gate is Extract<AskgateState, { readonly kind: "broken" }> => gate.kind === "broken"
  );
  const gated = gates.some((gate) => gate.kind === "on");
  if (!gated && broken === undefined) return undefined;
  const row = writeRoute(method, path.split("?", 1)[0] ?? "");
  const command =
    row === undefined ? NO_ROW : row.commands.map((name) => `dispatch ${name}`).join(" or ");
  return gated || broken === undefined
    ? new ApiGateRefusal(
        `dispatch api was not called: ${GATED_WHY}; make this write with ${command}`,
        { mode: "block", why: GATED_WHY }
      )
    : new ApiGateRefusal(
        `dispatch api was not called: AskGate's record for this session, ${broken.problem}, so whether AskGate reviews this session's Dispatch writes cannot be told, and dispatch api sends only GET here; make this write with ${command}`,
        { mode: "block", why: broken.problem }
      );
}

/**
 * Sends `args`, a `dispatch_api` call the contract's schema checked, through `client`, in `env`:
 * a GET of its path through `readRoute`, or its method with its body through `sendRoute`, which
 * a session whose AskGate reviews Dispatch writes refuses first (`gatedWriteRefusal`). Answers
 * Dispatch's JSON, printed for `jq`; a bodiless 204 or 205 prints as the empty text the client
 * reads it as.
 */
export async function sendApiCall(
  args: ToolArguments,
  env: Readonly<Record<string, string | undefined>>,
  client: DispatchClient
): Promise<DispatchToolResult> {
  const path = stringArg(args, "path");
  const method = optionalString(args, "method") as DispatchApiMethod | undefined;
  if (method !== undefined) {
    const refusal = gatedWriteRefusal(method, path, env);
    if (refusal !== undefined) throw refusal;
  }
  const answer =
    method === undefined
      ? await client.readRoute(path)
      : await client.sendRoute(method, path, optionalString(args, "body"));
  return {
    text: typeof answer === "string" ? answer : JSON.stringify(answer, null, 2),
    details: { path, ...defined("method", method) },
  };
}

/**
 * `dispatch api` under a host, as the command table runs it (`executeDispatchTool`); with none,
 * `runDispatchCli` calls `sendApiCall` itself, so the gate sits in the command, under a host and
 * with none. It names no owner, so it runs on the session's context alone.
 */
export async function apiCommand(context: SessionContext): Promise<DispatchToolResult> {
  return await sendApiCall(context.args, context.input.env ?? {}, context.newClient());
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

/**
 * Dispatch as `dispatch api` reads it where no host is found (`commandHost`), as in a GitHub
 * Actions job: from the environment alone, reading no envoy.json, its broker trade naming the
 * session of DISPATCH_SESSION_ID, else of AGENT_SECRETS_SESSION, else the session the job entry
 * fixes; or the usage error the command refuses with, each word of it the command's own.
 */
export async function unhostedDispatch(
  env: DispatchEnvironment
): Promise<{ readonly config: ActiveDispatchConfig } | { readonly problem: string }> {
  const resolution = await dispatchSettings(env, NO_FILE, {
    // biome-ignore lint/complexity/useLiteralKeys: claude-envoy's tsconfig refuses dot access on an index signature.
    session: env["DISPATCH_SESSION_ID"],
    tokenFiles: true,
  });
  if (resolution.enabled) return { config: resolution };
  // The off texts of an unset Dispatch or credential name an envoy.json, which a command with no
  // host does not read; any other says what was wrong with what is set, the token files' refusal
  // among them, which names DISPATCH_URL where it is unset beside them.
  return {
    problem:
      tokenFilesSet(env) === undefined && (env.DISPATCH_URL === undefined || namesNoCredential(env))
        ? `dispatch api was not called: ${NO_DISPATCH}`
        : `dispatch api was not called: dispatch config: ${resolution.error ?? resolution.notice}`,
  };
}
