// The long-lived clients of scripts/e2e/dispatch-broker.sh's rig
// (packages/daemon/internal/daemon/dispatch_broker_live_test.go): one process that holds the two
// TypeScript Dispatch clients a role in a pod writes through, each built as its own code builds
// it, and makes one write through either of them each time the test asks on standard input.
//
//   - `cli` is the client the `dispatch` command builds for one call (runDispatchCli,
//     packages/envoy-client/src/dispatch-cli.ts): the envoy.json the plugin hands its shell, else
//     the session's own by the omp rule, resolved with `sessionDispatchSettings` for the session
//     DISPATCH_SESSION_ID names, and the client `sessionContext` makes from it.
//   - `plugin` is the client the pi-envoy plugin builds for one request of its own
//     (`timedDispatchClient`, packages/pi-envoy/extensions/envoy.ts): `sessionDispatchClient`
//     (packages/pi-envoy/src/dispatch-session.ts) on the session's envoy.json by the omp rule, for
//     the session it acts as.
//
// Each is built again for each write, as each is for each call or request, with a deadline of its
// own. What lives as long as the process is the credential under them: under
// DISPATCH_TOKEN_BROKER=1 the broker token client `brokerCredential` keeps for the process
// (packages/envoy-client/src/broker-credential.ts), which trades the pod's subject token at the
// secrets broker, presents its token under DPoP and renews it ahead of its expiry, so a write long
// after the start presents a renewed token with no restart. Nothing of the environment is read but
// what the role's own is: the test starts this process with that environment, and nothing else.
//
//   DISPATCH_HOST=omp DISPATCH_SESSION_ID=<session> DISPATCH_URL=<dispatch> DISPATCH_TOKEN_BROKER=1 \
//     AGENT_SECRETS_URL=<broker> AGENT_SECRETS_MACHINE_IDENTITY=kubernetes \
//     AGENT_SECRETS_MACHINE_TOKEN_FILE=<subject token file> AGENT_SECRETS_KEY_DIR=<key dir> \
//     bun scripts/e2e/dispatch-broker-clients.ts
//
// The protocol is one JSON object per line each way. Once both clients' settings resolve it prints
// `{"ready":true,"session":"<session>"}`. Each line it reads asks one write,
// `{"id":"<id>","client":"cli"|"plugin","method":"<client method>","args":[...]}`, where the
// method is one of WRITES and the arguments are that method's own, the request body as Dispatch's
// route takes it; an object `{"$picture":"<path>"}` among them is that file's bytes as a PNG blob,
// which is how an upload's file travels. It answers each, in order, with
// `{"id":"<id>","ok":true,"answer":<what the method answered>}`, or for a write that failed
// `{"id":"<id>","ok":false,"message":"<the error's message>","status":<n>|null,"code":"<code>"|null}`:
// the status and code Dispatch answered, null where no answer came from Dispatch, as when the
// broker refused the trade and nothing was sent. A request it cannot read is answered the same
// way, naming why. It exits 0 once standard input ends, and 2 at the start when DISPATCH_HOST is not
// omp, DISPATCH_SESSION_ID is unset, or either client's Dispatch settings do not resolve.

import { readFileSync } from "node:fs";
import { sessionContext } from "../../packages/envoy-client/src/dispatch-commands/context";
import {
  type DispatchClient,
  DispatchServiceError,
} from "../../packages/envoy-client/src/dispatch-http";
import { messageFor } from "../../packages/envoy-client/src/errors";
import {
  handedConfigFile,
  sessionConfigFile,
  sessionDispatchSettings,
} from "../../packages/envoy-client/src/session-settings";
import { sessionDispatchClient } from "../../packages/pi-envoy/src/dispatch-session";

/** The client methods a request may name: every write a role makes that the rig drives. */
const WRITES: Record<string, true> = {
  agentArtifact: true,
  artifact: true,
  ask: true,
  claimIssue: true,
  comment: true,
  docEdit: true,
  message: true,
  recordResult: true,
  releaseIssueClaim: true,
  writeHandoff: true,
};

/** How long one request of the plugin's has to answer: a tool call's own deadline. */
const PLUGIN_REQUEST_MS = 60_000;

function refuse(why: string): never {
  console.error(`dispatch-broker-clients: ${why}`);
  process.exit(2);
}

if (process.env.DISPATCH_HOST !== "omp") {
  refuse("DISPATCH_HOST is not omp; the role these clients belong to runs in an Oh My Pi pane");
}
const session = process.env.DISPATCH_SESSION_ID ?? "";
if (session === "") {
  refuse("DISPATCH_SESSION_ID names the session both clients act as, and it is not set");
}
const cwd = process.cwd();

/** The client the `dispatch` command builds for one call, as runDispatchCli builds it. */
function cliClient(): DispatchClient {
  const env = process.env;
  const file = handedConfigFile(env) ?? sessionConfigFile("omp", env);
  if (file.path === null) throw new Error(`dispatch: ${file.error}`);
  const { dispatch: resolution } = sessionDispatchSettings(file, env, { cwd, session });
  if (!resolution.enabled) {
    throw new Error(
      resolution.error === null
        ? `dispatch: ${resolution.notice}`
        : `dispatch: dispatch config: ${resolution.error}`
    );
  }
  return sessionContext(
    {
      tool: "dispatch_comment",
      args: {},
      cwd,
      host: "omp",
      sessionId: session,
      config: resolution,
      env,
    },
    { problems: [], owner: { args: {}, ref: null, owner: null }, args: {}, pictures: [] }
  ).newClient();
}

/** The client pi-envoy's extension builds for one request of its own, as timedDispatchClient
 *  builds it, acting as `session`. */
function pluginClient(): DispatchClient {
  const made = sessionDispatchClient(
    sessionConfigFile("omp", process.env),
    cwd,
    AbortSignal.timeout(PLUGIN_REQUEST_MS),
    () => session
  );
  if (made === undefined) throw new Error("the plugin's session has no Dispatch configured");
  return made.client;
}

/** A request's arguments as the method takes them: each `{"$picture": path}` is that file's
 *  bytes as a PNG blob, and everything else is as it was sent. */
function decoded(value: unknown): unknown {
  if (Array.isArray(value)) return value.map(decoded);
  if (value === null || typeof value !== "object") return value;
  const fields = value as Record<string, unknown>;
  if (typeof fields.$picture === "string") {
    return new Blob([readFileSync(fields.$picture)], { type: "image/png" });
  }
  return Object.fromEntries(Object.entries(fields).map(([key, field]) => [key, decoded(field)]));
}

interface WriteRequest {
  readonly id: string;
  readonly client: "cli" | "plugin";
  readonly method: string;
  readonly args: unknown[];
}

function print(line: Record<string, unknown>): void {
  process.stdout.write(`${JSON.stringify(line)}\n`);
}

/** One write, made through a client built for it, answered in the protocol's one line. */
async function write(request: WriteRequest): Promise<void> {
  const { id } = request;
  try {
    if (request.client !== "cli" && request.client !== "plugin") {
      throw new Error(`client ${JSON.stringify(request.client)} is neither cli nor plugin`);
    }
    if (!Object.hasOwn(WRITES, request.method)) {
      throw new Error(`${JSON.stringify(request.method)} is not a write these clients make`);
    }
    if (!Array.isArray(request.args)) throw new Error("args is not a list");
    const client = request.client === "cli" ? cliClient() : pluginClient();
    const method = (client as unknown as Record<string, (...args: unknown[]) => Promise<unknown>>)[
      request.method
    ];
    if (method === undefined) throw new Error(`the client has no method ${request.method}`);
    const answer = await method.apply(client, decoded(request.args) as unknown[]);
    print({ id, ok: true, answer: answer ?? null });
  } catch (error) {
    const fromDispatch = error instanceof DispatchServiceError;
    print({
      id,
      ok: false,
      message: messageFor(error),
      status: fromDispatch ? error.status : null,
      code: fromDispatch ? error.code : null,
    });
  }
}

// Both clients' settings resolve before the test is told this process is ready: a role whose
// environment names no Dispatch is refused here, rather than at its first write.
try {
  cliClient();
  pluginClient();
} catch (error) {
  refuse(messageFor(error));
}
print({ ready: true, session });

for await (const line of console) {
  if (line.trim() === "") continue;
  let request: WriteRequest;
  try {
    request = JSON.parse(line) as WriteRequest;
  } catch (error) {
    print({ id: null, ok: false, message: `a request that is no JSON: ${messageFor(error)}` });
    continue;
  }
  await write(request);
}
