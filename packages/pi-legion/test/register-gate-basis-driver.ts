// The tool side of k8's live proof (dispatch://INFRA-3320, R1): this checkout's `legion` tool,
// built as a root architect's pane builds it, makes one call against a running daemon and a
// running Dispatch, and prints what came of it. `TestRegisterGateBasisToolLive`
// (packages/daemon/internal/daemon/register_gate_basis_live_test.go, which
// scripts/e2e/register-gate-basis.sh builds with `-tags e2e`) boots both, runs this file under bun
// with its input on stdin, and reads the answer this file prints.
//
// Nothing of the call path is stood in for. The daemon client is the plugin's own
// (`createLegionDaemonClient`, which `extensions/legion.ts` boots every session through) at the
// daemon's API, and the Dispatch client is envoy-client's `DispatchClient` at the Dispatch server,
// with the bearer a pane's DISPATCH_TOKEN carries. The session is the claim the daemon registered
// for the root's architect, which the tool mints each grant from in this process
// (`mintClaimGrant`), as a pane's tool does. Each client's fetch records the method and the path of
// every request before it sends it, so the answer lists the requests the call made, and a call the
// tool refused before it reached the daemon lists no daemon request.
//
// Input, one JSON object on stdin: `daemonUrl`, `dispatchUrl`, `dispatchToken`, `claim` (the
// claim's session id, tree, issue, role, secret, claim token and generation) and `call` (the
// tool's parameters). Output, one JSON object on stdout: `isError`, `text` (the tool's answer as
// the agent reads it), and `daemonRequests` and `dispatchRequests`, each request as
// `<METHOD> <path>`, in the order they were sent.
//
// The file builds the tool through `createLegionTool` alone, so it runs against the tool of
// another checkout as well: copied into that checkout's packages/pi-legion/test/, it drives that
// checkout's tool, which is how k8's negative control drives the tool of the commit the branch
// starts from (scripts/e2e/README.md, "register-gate-basis.sh").
import { bearerCredential } from "@legion/envoy-client/bearer";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { createLegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { z } from "zod";
import { createLegionTool } from "../src/tools";

const Input = z.object({
  daemonUrl: z.string(),
  dispatchUrl: z.string(),
  dispatchToken: z.string(),
  claim: z.object({
    sessionID: z.string(),
    tree: z.string(),
    issue: z.string(),
    role: z.literal("architect"),
    secret: z.string(),
    claimToken: z.string(),
    generation: z.number(),
  }),
  call: z.record(z.string(), z.unknown()),
});

/** `fetch`, recording each request's method and path in `requests` before sending it. */
function recordingFetch(requests: string[]): typeof fetch {
  return ((input: string | URL | Request, init?: RequestInit) => {
    const request = input instanceof Request ? input : undefined;
    const url = new URL(request?.url ?? String(input));
    requests.push(`${init?.method ?? request?.method ?? "GET"} ${url.pathname}${url.search}`);
    return fetch(input, init);
  }) as typeof fetch;
}

const input = Input.parse(JSON.parse(await Bun.stdin.text()));
const daemonRequests: string[] = [];
const dispatchRequests: string[] = [];
const daemon = createLegionDaemonClient(input.daemonUrl, recordingFetch(daemonRequests));
const board = new DispatchClient(
  input.dispatchUrl,
  bearerCredential(input.dispatchToken),
  recordingFetch(dispatchRequests)
);
const unexpected = async (what: string): Promise<never> => {
  throw new Error(`a register_gate call reads no ${what}`);
};

// The tool reads the host's zod alone of the host's API (`toolSchema`), and of the session's
// context nothing: `session` answers the claim whatever context it is handed.
const pi = { zod: z } as unknown as PiApi;
const context: SessionContext = {
  cwd: process.cwd(),
  hasUI: true,
  setInterval: () => undefined,
  setTimeout: () => undefined,
  sessionManager: {
    getSessionId: () => input.claim.sessionID,
    getSessionFile: () => undefined,
    ensureOnDisk: async () => undefined,
    getEntries: () => [],
  },
  ui: { notify: () => undefined },
};

const tool = createLegionTool({
  pi,
  daemon: () => daemon,
  session: () => ({ kind: "architect", ...input.claim }),
  controllerGrant: () => unexpected("controller grant"),
  onPhaseCompleted: () => undefined,
  onUpdateAnswered: () => undefined,
  handoffReport: () => unexpected("handoff report"),
  branchHead: () => unexpected("branch head"),
  dispatch: () => board,
});
const result = await tool.execute("k8", input.call, undefined, undefined, context);
const text = result.content
  .flatMap((block) => (block.type === "text" ? [block.text] : []))
  .join("");
process.stdout.write(
  `${JSON.stringify({ isError: result.isError === true, text, daemonRequests, dispatchRequests })}\n`
);
