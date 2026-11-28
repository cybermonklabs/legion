/**
 * The plugin's own daemon client, driven against a real daemon: one call of the `legion` tool's
 * client per run, as `packages/pi-legion/src/tools.ts` and `packages/pi-shared/src/legion-phase-ops.ts`
 * make it. The Go driver `TestOwnerStepsClientLive`
 * (`packages/daemon/internal/daemon/owner_steps_client_live_test.go`, run by
 * `scripts/e2e/owner-steps-client.sh`) boots the whole daemon and runs this script once per call:
 *
 *   OWNER_STEPS_CLIENT_REQUEST='<json>' [OWNER_STEPS_CLIENT_ROOT=<checkout>] bun scripts/e2e/owner-steps-client.ts
 *
 * OWNER_STEPS_CLIENT_ROOT names the checkout whose `packages/pi-shared` the client is imported
 * from: this checkout by default, or a tree of an earlier daemon API contract (a plugin of the
 * release before), with its own `bun install`, so its `@legion/contracts` schemas read the answers.
 *
 * The request: `{ daemon, claim, call, args, report? }`. `daemon` is the daemon's base URL;
 * `claim` the `ClaimCapability` of the session that calls (session id, secret, tree, issue, role,
 * claim token, generation). `call` is one of:
 *   - `register_plan` (args `issue`, `artifactId`, `version`): `client.planRegister` with a grant
 *     minted by `mintClaimGrant`, as tools.ts's `register_plan` posts it;
 *   - `stop_task` (args `issue`, `task`): `client.taskStop`, as tools.ts's `stop_task`;
 *   - `handoff_complete` (args as the tool's: `summary` and `return_to`, with no verdict on a phase
 *     completion): the phase op itself (`executePhaseOp`), with `report` as what the pane's
 *     handoff record answers, the tester's naming the head it tested as `commit`;
 *   - `complete` (args `summary`, `verdict`): `client.handoffComplete` with a fresh grant and
 *     `report`, for a client whose phase op cannot run (one whose state read refuses), such as
 *     the plugin of the release before, which still sends its tester's verdict;
 *   - `state` (args `issue`, `project`): `client.state()`, answering that issue's record, its
 *     `admission.held` rows and the project's settings;
 *   - `register` (args as `RegisterInput`: `bootToken`, `sessionId`, `ompSessionFile`, `agentId`,
 *     `pluginContract`): `client.register`, the pane's own registration, answering the claim's
 *     role and generation (never its secret).
 * It prints one JSON line: `{"ok":true,"answer":…,"methods":[…]}` or
 * `{"ok":false,"error":<class name>,"status":…,"code":…,"message":…,"methods":[…]}`, where
 * `methods` lists the client's own calls (a client of an earlier contract has no planRegister).
 * Exit 0 for either; exit 2 only when the request itself cannot be read.
 */
import { resolve } from "node:path";
// By path: the root package does not depend on @legion/contracts. It is this checkout's, whatever
// tree the client comes from: a tree of a commit before daemonContract() has a manifest that
// declares its contract, which daemonContract() answers first.
import { daemonContract } from "../../packages/contracts/src/daemon-contract";

interface Request {
  readonly daemon: string;
  readonly claim: Record<string, unknown>;
  readonly call: string;
  readonly args: Record<string, unknown>;
  readonly report?: Record<string, unknown>;
}

const raw = process.env.OWNER_STEPS_CLIENT_REQUEST;
if (raw === undefined || raw === "") {
  console.error("owner-steps-client: OWNER_STEPS_CLIENT_REQUEST is unset");
  process.exit(2);
}
const request = JSON.parse(raw) as Request;
const root = resolve(process.env.OWNER_STEPS_CLIENT_ROOT || resolve(import.meta.dir, "../.."));
// The client's tree is chosen at run time (this checkout, or a tree of an earlier contract), so its
// modules cannot be imported statically.
const clientModule = await import(resolve(root, "packages/pi-shared/src/daemon-client.ts"));
const phaseOps = await import(resolve(root, "packages/pi-shared/src/legion-phase-ops.ts"));
// Each client states the contract its own tree's plugin speaks, as the plugin passes its own
// (`daemonContract(pkg, import.meta.dir)`, `packages/pi-legion/src/claim-session.ts`): that tree's
// manifest's `legion.daemonApiVersion` where it declares one, else its frozen list's greatest
// number plus its count of entry fixtures, counted from that tree's own `packages/pi-legion`. The
// daemon serves each caller the shape of the contract it states.
const manifest = await import(resolve(root, "packages/pi-legion/package.json"), {
  with: { type: "json" },
});
const client = clientModule.createLegionDaemonClient(request.daemon, {
  pluginContract: daemonContract(manifest.default, resolve(root, "packages/pi-legion")),
});
const methods = Object.keys(client).sort();
const claim = request.claim;
const args = request.args;

async function call(): Promise<unknown> {
  switch (request.call) {
    case "register_plan":
      return client.planRegister({
        grantId: await phaseOps.mintClaimGrant(client, claim),
        issue: args.issue,
        artifactId: args.artifactId,
        version: args.version,
      });
    case "stop_task":
      return client.taskStop({
        grantId: await phaseOps.mintClaimGrant(client, claim),
        issue: args.issue,
        task: args.task,
      });
    case "handoff_complete":
      return phaseOps.executePhaseOp(
        {
          daemon: client,
          claim,
          handoffReport: async () => request.report ?? {},
          branchHead: async () => "",
        },
        { op: "handoff_complete", ...args }
      );
    case "complete":
      return client.handoffComplete({
        grantId: await phaseOps.mintClaimGrant(client, claim),
        summary: args.summary,
        verdict: args.verdict ?? "",
        ...(request.report ?? {}),
      });
    case "register": {
      const registration = await client.register(args);
      return { role: registration.role, generation: registration.generation };
    }
    case "state": {
      const state = await client.state();
      const issue = String(args.issue);
      const held = (state.admission?.held ?? []).filter(
        (row: { issue?: string }) => row.issue === issue
      );
      return {
        issue: state.issues[issue],
        held,
        settings: state.settings?.projects?.[String(args.project)],
      };
    }
    default:
      throw new Error(`owner-steps-client: no call ${request.call}`);
  }
}

try {
  const answer = await call();
  console.log(JSON.stringify({ ok: true, answer: answer ?? null, methods }));
} catch (error) {
  const e = error as {
    name?: string;
    status?: number;
    code?: string;
    message?: string;
  };
  console.log(
    JSON.stringify({
      ok: false,
      error: e?.name ?? typeof error,
      status: e?.status ?? null,
      code: e?.code ?? null,
      message: e?.message ?? String(error),
      methods,
    })
  );
}
