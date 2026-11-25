// The tool side of k32's live proof (dispatch://INFRA-4730, decision 13): a checkout's `legion`
// tool, built as an architect's pane builds it, makes one call against a running daemon and a
// running Dispatch while holding the claim as the plugin's registration answered it, and prints
// what came of it. `TestAMemberThatBecomesARootIsRelaunchedAndRegisters`
// (packages/daemon/internal/daemon/member_root_live_test.go, which
// scripts/e2e/member-becomes-root.sh builds with `-tags e2e`) boots both, runs this file under bun
// in this checkout and in the root of the oldest plugin contract in the fleet
// (scripts/e2e/lib/old-plugin-root.sh copies it there), with its input on stdin, and reads the
// answer this file prints.
//
// The file runs against both tools, so it builds each from the parts both have. The tool is
// `createLegionTool`, handed every dependency either version reads: the Dispatch client this
// checkout's tool looks documents up through and the document lookup the older one takes in its
// place, both reading the same Dispatch. The daemon client is the plugin's own
// (`createLegionDaemonClient`): this checkout's takes its options (the plugin's contract and the
// fetch) as an object, and the older one takes the fetch alone, so it is handed the recording
// fetch carrying those options as its own fields, which each version reads its own way.
//
// Input, one JSON object on stdin: `daemonUrl`, `dispatchUrl`, `dispatchToken`, `claim` (the
// claim's session id, tree and issue as the registration answered them, role, secret, claim token
// and generation) and `call` (the tool's parameters). Output, one JSON object on stdout: `isError`,
// `text` (the tool's answer as the agent reads it), and `daemonRequests` and `dispatchRequests`,
// each request as `<METHOD> <path>`, in the order they were sent.
import { bearerCredential } from "@legion/envoy-client/bearer";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { createLegionDaemonClient, type LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import type { PiApi, RegisteredTool, SessionContext } from "@legion/pi-shared/pi-types";
import { z } from "zod";
import pkg from "../package.json";
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
const daemonFetch = recordingFetch(daemonRequests);
// The plugin's own contract: the `legion.daemonApiVersion` the manifest declares, as every root
// from before dispatch://INFRA-1569 does, else the number `daemonContract` counts for this
// checkout, whose manifest declares none. An older root has no such module, so it is loaded only
// where the manifest declares no number.
const manifest: { name: string; legion?: { daemonApiVersion?: number } } = pkg;
const pluginContract =
  manifest.legion?.daemonApiVersion ??
  (await import("@legion/contracts/daemon-contract")).daemonContract(pkg, import.meta.dir);
// One value serves both signatures: a fetch, which the older client calls, carrying the options
// object this checkout's client reads, the plugin's own contract among them.
const daemonOptions = Object.assign(daemonFetch, {
  pluginContract,
  fetch: daemonFetch,
});
const daemon = (createLegionDaemonClient as (url: string, options: unknown) => LegionDaemonClient)(
  input.daemonUrl,
  daemonOptions
);
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

const deps = {
  pi,
  daemon: () => daemon,
  session: () => ({ kind: "architect", ...input.claim }),
  controllerGrant: () => unexpected("controller grant"),
  onPhaseCompleted: () => undefined,
  onUpdateAnswered: () => undefined,
  handoffReport: () => unexpected("handoff report"),
  branchHead: () => unexpected("branch head"),
  dispatch: () => board,
  resolveDocument: () => unexpected("document lookup: the call names the document's id"),
};
const tool = (createLegionTool as (deps: unknown) => RegisteredTool)(deps);
const result = await tool.execute("k32", input.call, undefined, undefined, context);
const text = result.content
  .flatMap((block) => (block.type === "text" ? [block.text] : []))
  .join("");
process.stdout.write(
  `${JSON.stringify({ isError: result.isError === true, text, daemonRequests, dispatchRequests })}\n`
);
