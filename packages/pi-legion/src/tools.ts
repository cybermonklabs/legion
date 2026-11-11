import { GATED_STEPS, LEGION_ROLES, type LegionRole } from "@legion/contracts";
import {
  LegionGateRegisterRequest,
  type LegionGrant,
  LegionInstant,
  type LegionPhase,
} from "@legion/contracts/legion-api";
import {
  type DaemonRestartRetry,
  type LegionDaemonClient,
  retryWhileDaemonRestarts,
} from "@legion/pi-shared/daemon-client";
import type { HandoffReport } from "@legion/pi-shared/handoff-record";
import {
  type ClaimCapability,
  executePhaseOp,
  isPhaseOp,
  legionToolDescription,
  mintClaimGrant,
  PHASE_OP_SENTENCES,
  phaseOpParameters,
} from "@legion/pi-shared/legion-phase-ops";
import type { PiApi, RegisteredTool, SessionContext, ToolResult } from "@legion/pi-shared/pi-types";
import { toolFailure, toolSuccess } from "@legion/pi-shared/tool-result";

export type LegionToolRole = "architect" | "phase-worker" | "controller";

/** A root architect's, a sub-architect's or a phase worker's session: its claim, which mints its
 * grants on `/grants`' session form. `role` is the claim's Legion role (a sub-architect's is
 * `architect`); `kind` is what the tool's operation sets are keyed by. */
export interface LegionClaimToolSession extends ClaimCapability {
  readonly kind: "architect" | "phase-worker";
}

/** The controller's session: no tree, no issue; its grants are minted with its registration's
 * secret (`controllerGrant`, the `/grants` controller-session form). */
export interface LegionControllerToolSession {
  readonly kind: "controller";
  readonly sessionID: string;
}

export type LegionToolSession = LegionClaimToolSession | LegionControllerToolSession;

/** Each session kind's operations, in the order the tool's `op` enum lists them. The phase ops
 * (`@legion/pi-shared/legion-phase-ops`) are an architect's too. */
const OPERATIONS: Readonly<Record<LegionToolRole, readonly string[]>> = {
  architect: [
    "register_gate",
    "release_children",
    "request_backward_move",
    "retry_or_escalate",
    "sign_off",
    "close_root",
    "park_child",
    "rerun_child",
    "read_record",
    "read_release_records",
    "handoff_complete",
    "ready_for_update",
  ],
  "phase-worker": [
    "request_backward_move",
    "read_record",
    "read_release_records",
    "handoff_complete",
    "ready_for_update",
  ],
  controller: [
    "read_state",
    "set_status",
    "fresh_session",
    "compact",
    "set_merge_jump",
    "clear_merge_jump",
    "set_merge_holds",
  ],
};

/** The fields each operation beyond the phase ops takes; a phase op's are `PhaseOpArguments`'. */
const OPERATION_FIELDS: Readonly<Record<string, readonly string[]>> = {
  register_gate: ["issue", "artifactId", "version", "step"],
  release_children: ["issues"],
  retry_or_escalate: ["issue", "decision"],
  sign_off: ["issue"],
  close_root: ["issue", "reason"],
  park_child: ["issue"],
  rerun_child: ["issue"],
  read_state: [],
  set_status: ["issue", "status"],
  fresh_session: ["issue", "role"],
  compact: ["issue", "role"],
  set_merge_jump: ["issue", "reason"],
  clear_merge_jump: ["issue"],
  set_merge_holds: ["holds"],
};

/** One hold of the list `set_merge_holds` sends, as the daemon client takes it
 * (`contracts`' `LegionMergeHoldsRequest`). */
type MergeHoldInput = Parameters<LegionDaemonClient["mergeHolds"]>[0]["holds"][number];

const ISSUE_STATUSES = ["todo", "backlog", "icebox"] as const;

const jsonSuccess = (details: Readonly<Record<string, unknown>>): ToolResult =>
  toolSuccess(JSON.stringify(details), details);

/** The tool's one object schema, `op` choosing the operation; its fields and the `op` values are
 * in the order `tools.test.ts` pins. */
function toolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  const phase = phaseOpParameters(z);
  return z.object({
    op: z.enum([...OPERATIONS.architect, ...OPERATIONS.controller]),
    issue: phase.issue,
    since: phase.since,
    artifactId: z
      .string()
      .describe(
        "register_gate's document for the step it registers: its artifact id, slug, or filename, as a dispatch command's --artifact takes it"
      )
      .optional(),
    version: z.number().optional(),
    // The steps are `GATED_STEPS`, the one tuple the contracts package holds them in, so this
    // schema and the daemon's own (`LegionGateRegisterRequest.shape.step`) admit the same words
    // and a step added to the contract reaches both. The tuple is what crosses, not a zod
    // schema: this schema is built with the host's own zod instance (`pi.zod`).
    step: z
      .enum(GATED_STEPS)
      .describe(
        "register_gate's design-gate step: decisions for the issue's decisions document, criteria for its Verification document, spec for its primary document. A deployment that gates one step takes the registration without it"
      )
      .optional(),
    issues: z.array(z.string()).optional(),
    to: phase.to,
    reason: phase.reason,
    decision: z.enum(["retry", "escalate"]).optional(),
    summary: phase.summary,
    verdict: phase.verdict,
    ready: phase.ready,
    wake_at: phase.wake_at,
    round: phase.round,
    status: z.enum(ISSUE_STATUSES).optional(),
    role: z.enum(LEGION_ROLES).optional(),
    holds: z
      .array(
        z.object({
          paths: z.array(z.string()),
          repository: z.string().optional(),
          reason: z.string(),
          endsAt: z.string(),
        })
      )
      .describe(
        "set_merge_holds' whole hold list, which replaces the list that stands. Each hold names paths, one or more regular expressions matched unanchored against the paths a pull request changes, a reason, and endsAt, an RFC 3339 time with seconds later than now. A hold may also name a repository as owner/name, one this project serves. A hold that names no repository holds its paths in every repository the project serves. An empty list clears every hold"
      )
      .optional(),
  });
}

function requiredString(
  parameters: Record<string, unknown>,
  operation: string,
  name: string
): string {
  const value = parameters[name];
  if (typeof value !== "string" || value.trim() === "")
    throw new Error(`${operation} requires ${name}`);
  return value;
}

function assertOperationInput(parameters: Record<string, unknown>, operation: string): void {
  const fields = OPERATION_FIELDS[operation];
  if (fields === undefined) throw new Error(`Unsupported legion operation: ${operation}`);
  for (const name of Object.keys(parameters)) {
    if (name !== "op" && !fields.includes(name)) {
      throw new Error(`${operation} does not accept field "${name}"`);
    }
  }
}

/** The holds `set_merge_holds` is to send, every value checked here before the grant is minted,
 * as the daemon checks them before it redeems one (`api.server.holdList`). Each refusal names
 * the hold by its place in the list the agent wrote, since a hold has no identifier the two
 * sides share. A list with no hold in it is the request that clears every hold, so it passes. */
function holdList(parameters: Record<string, unknown>): MergeHoldInput[] {
  const holds = parameters.holds;
  if (!Array.isArray(holds)) throw new Error("set_merge_holds requires holds");
  return holds.map((hold, index) => {
    const where = `hold ${index + 1} of set_merge_holds`;
    if (typeof hold !== "object" || hold === null || Array.isArray(hold)) {
      throw new Error(`${where} is no hold: name its paths, its reason and when it ends`);
    }
    const named = hold as Record<string, unknown>;
    const paths = named.paths;
    if (!Array.isArray(paths) || paths.length === 0) {
      throw new Error(`${where} names no path: a hold with no path keeps back nothing`);
    }
    const taken: string[] = [];
    for (const path of paths) {
      if (typeof path !== "string" || path.trim() === "") {
        throw new Error(
          `${where} names an empty path, which matches every file of every pull request`
        );
      }
      taken.push(path);
    }
    const reason = requiredString(named, where, "reason");
    const endsAt = LegionInstant.safeParse(named.endsAt);
    if (!endsAt.success) {
      throw new Error(
        `${where} needs endsAt, an RFC 3339 time with seconds, as 2026-11-02T09:00:00Z is`
      );
    }
    const repository = named.repository;
    if (repository !== undefined && (typeof repository !== "string" || repository.trim() === "")) {
      throw new Error(
        `${where} names an empty repository: name one this project serves, or leave it out to hold its paths in every one of them`
      );
    }
    return {
      paths: taken,
      ...(repository === undefined ? {} : { repository }),
      reason,
      endsAt: endsAt.data,
    };
  });
}

/** The daemon's role-local workflow surface: no operation can schedule a worker. Every operation
 * that writes mints its own grant in-process and posts it with the request; nothing is written
 * to the pane. The phase ops run on the executor every harness shares (`executePhaseOp`):
 * `handoff_complete` belongs to every session but the root architect's (a phase worker, and a
 * sub-architect: an architect whose issue is not its tree), and finds what it reports in the pane
 * first (`handoffReport`); `read_state`, `set_status`, the two claim recoveries and the three
 * merge marks belong to the controller. */
export function createLegionTool(deps: {
  readonly pi: PiApi;
  readonly daemon: () => LegionDaemonClient;
  readonly session: (context: SessionContext) => LegionToolSession;
  /** Mints the controller's grant, authenticated by its registration's secret. */
  readonly controllerGrant: (sessionId: string) => Promise<LegionGrant>;
  /** Told of each `handoff_complete` that succeeded: the session's phase is complete. */
  readonly onPhaseCompleted: (context: SessionContext) => void;
  /** Told of each `ready_for_update` that succeeded: the session has answered the daemon's
   * update task on its pod, and its phase stays open. */
  readonly onUpdateAnswered: (context: SessionContext) => void;
  /** What a completion of `phase` by `role` reports beside its summary (`findHandoffReport`): for
   * a file-backed phase the role works, the newest Dispatch handoff record of the phase, with the
   * pushed tip of `legion/<issue>@origin` for the implementer; otherwise the commit the workspace
   * stands on. Throws naming the remedy when the phase has no record, Dispatch does not answer, or
   * the implementer's work is not pushed. */
  readonly handoffReport: (phase: LegionPhase, role: LegionRole) => Promise<HandoffReport>;
  /** The head of `legion/<issue>@origin` an update signal reports, read with the pane's jj
   * (`findPushedBranchHead`), and `""` when the branch is not on the remote. */
  readonly branchHead: () => Promise<string>;
  /** The id of the document `issue` carries under `reference` (`spec`, a slug, or a filename),
   * looked up in Dispatch as a `dispatch` command's `--artifact` is; throws naming the reference
   * when none matches, or when it names two documents. */
  readonly resolveDocument: (issue: string, reference: string) => Promise<string>;
  /** The clock `handoff_complete` and `sign_off` wait out a daemon restart on; a test's own. */
  readonly restartClock?: Pick<DaemonRestartRetry, "now" | "sleep">;
}): RegisteredTool {
  const {
    pi,
    daemon,
    session,
    controllerGrant,
    onPhaseCompleted,
    onUpdateAnswered,
    handoffReport,
    branchHead,
    resolveDocument,
    restartClock,
  } = deps;
  return {
    name: "legion",
    label: "legion",
    // No sentence on read_record, whose name says what it reads; the shared sentence on
    // read_release_records before handoff_complete's contract, since the records it answers are
    // not read from its name; after that contract the shared sentence on request_backward_move
    // (an architect's names the issue it returns from accepting), then the architect's sign_off
    // and the controller's operations: the exact text tools.test.ts pins, so a change to the
    // shared template cannot reach an Oh My Pi agent unnoticed.
    description: legionToolDescription({
      envoyPublish: "envoy_publish",
      before: PHASE_OP_SENTENCES.readReleaseRecords,
      after:
        PHASE_OP_SENTENCES.requestBackwardMove +
        "sign_off is the architect's acceptance of an issue in accepting, moving it to retro; the daemon refuses it in every " +
        "other phase (SIGNOFF_OUTSIDE_ACCEPTING) and while an issue beneath it is still in the workflow " +
        "(SIGNOFF_CHILD_IN_WORKFLOW, naming each one), since a parent's head proves its children's merged work: take each one " +
        "to done, or out of the workflow with park_child, and sign off again. close_root is refused the same way and with the " +
        "same code while an issue beneath the root is still in the workflow, since closing it would stop that work mid-turn. " +
        "release_children and rerun_child are refused for a child whose ancestor is in retro, merging or awaiting_merge " +
        "(CHILD_UNDER_ACCEPTED_PARENT, naming the ancestor), since that ancestor's approved head cannot carry work released " +
        "under it now: wait for the ancestor to close, then release it. " +
        "The controller's read_state returns the daemon's whole state and set_status moves an issue to todo, backlog or icebox. " +
        "fresh_session (issue, role) has one claim leave the session it runs in behind and start again in a new one: the daemon " +
        "stops the agent if it runs, forgets the session, and starts the claim, which loses everything that session held. A " +
        "phase worker that comes back owing no task is started through its issue's current phase, so it has that phase's work. " +
        "The answer is empty when the claim came back and nothing else is to be said, and names why nothing restarted the " +
        "phase (phaseNotRestarted) when the claim came back owing no task and the workflow refused that restart: the claim is " +
        "running, and no work was handed to it. " +
        "The daemon refuses it for a claim it records as working, which may be in a turn " +
        "(CLAIM_IN_A_TURN), for a claim left working over a process the daemon's last report " +
        "of it found gone, where no turn is running (CLAIM_IN_A_TURN as well), for a claim a " +
        "suspension is held over, in whatever state the hold found it (CLAIM_IN_A_TURN where " +
        "the claim is working, CLAIM_UNSETTLED where it is not), for a claim between states — " +
        "one queued, one whose launch is in flight, and one left working under a relaunch the " +
        "wake pacing parked (CLAIM_UNSETTLED) — and for role controller. " +
        "compact (issue, role) shortens one claim's session instead: the daemon has Oh My Pi write a text summary of the " +
        "session so far, and the claim keeps its session, its workspace and everything it knows of its issue and is sent " +
        "whatever task it owes again. Try compact before fresh_session for a claim that stays stuck. The answer says " +
        "whether the session was compacted and, when it was not, why; the claim is back either way. The daemon refuses " +
        "it for a claim that is suspended, failed or retired (CLAIM_NOT_RUNNING: fresh_session " +
        "reaches that one), for a claim it records as working, which may be in a turn " +
        "(CLAIM_IN_A_TURN), for a claim left working over a process the daemon's last report " +
        "of it found gone, where no turn is running (CLAIM_IN_A_TURN), for a claim a " +
        "suspension is held over (CLAIM_IN_A_TURN where the claim is working, CLAIM_UNSETTLED " +
        "where it is not), for a claim between states — one queued, one whose launch is in " +
        "flight, one whose agent has registered and is not ready yet, which fresh_session " +
        "reaches, and one left working under a relaunch the wake pacing parked " +
        "(CLAIM_UNSETTLED) — for a claim whose role runs on Claude Code (CLAIM_NOT_ON_OMP) " +
        "and for role controller. " +
        "Each refusal says what the claim does now and which operation reaches each state. It " +
        "names no moment to come back at: ask again later, and read the claim's state in " +
        "read_state. " +
        "set_merge_jump (issue, reason) marks one issue so that the next merge the daemon submits for it goes in at the " +
        "head of GitHub's merge queue, ahead of the pull requests already in it. The reason stays in the record of the " +
        "jump after the mark is spent, so the account of a pull request that went ahead of the queue names the decision. " +
        "One issue carries at most one mark, and a second set of the same issue replaces the first. Only a merge that " +
        "goes into a merge queue spends the mark, which is a merge whose base branch requires one. Where the base " +
        "requires no merge queue there is no queue to jump: the daemon submits that merge as it always does, and the " +
        "mark stands until clear_merge_jump takes it off or the issue leaves the workflow. " +
        "The daemon refuses an issue it does not record " +
        "(ISSUE_NOT_RECORDED) and a call that leaves the reason out. A jump GitHub refuses does not stop the merge: the " +
        "daemon submits it without the jump and records what GitHub answered. " +
        "clear_merge_jump (issue) takes an issue's mark off before the merge it was about is submitted. It answers the " +
        "same whether a mark went or no mark was there, and the daemon refuses an issue it does not record " +
        "(ISSUE_NOT_RECORDED). " +
        "set_merge_holds (holds) replaces the project's whole merge hold list. While a hold stands, the daemon submits " +
        "no merge whose pull request changes a file one of that hold's paths matches, and the issue waits in " +
        "awaiting_merge with its architect told which hold holds it. A hold has no identifier of its own, so one call " +
        "carries the whole list: send the list without a hold to drop that hold, and an empty list to clear every hold. " +
        "Each hold names paths, regular expressions matched unanchored against the paths the pull request changes " +
        "(HOLD_WITHOUT_PATH for a hold that names none, INVALID_HOLD_PATH for a path that is no regular expression), a " +
        "reason, which the architect of a held issue is told (HOLD_WITHOUT_REASON), and endsAt, an RFC 3339 time later " +
        "than now (HOLD_ALREADY_ENDED). A hold may also name a repository as owner/name, and this project must serve it " +
        "(REPOSITORY_NOT_SERVED). A hold that names no repository holds its paths in every repository the project " +
        "serves. A hold ends by itself at endsAt, and taking a hold off makes every merge it was holding back due at " +
        "once. The daemon checks every hold before it writes anything, so a list with one bad hold in it leaves the list " +
        "that stands exactly as it was. " +
        "All three need the controller's grant, which this session mints, and the daemon refuses a call without one " +
        "(CONTROLLER_REQUIRED). Each answers an empty object, and read_state reads back what they wrote: mergeJumps the " +
        "marks that still stand, mergeHolds the hold list, mergeHeld the issues those holds are keeping back, and " +
        "mergeJumpRecords the newest jump of each repository, with the queue it displaced and GitHub's answer where it " +
        "refused one. ",
    }),
    defaultInactive: true,
    parameters: toolSchema(pi),
    execute: async (_id, parameters, signal, onUpdate, context) => {
      // The three writes the daemon applies at most once however often they arrive
      // (handoff_complete, sign_off, register_gate) wait out its restart until the agent's abort.
      // The retry notice goes out once as a partial result, which Oh My Pi shows in its TUI and
      // RPC event stream; the model sees only the final result.
      const restart: DaemonRestartRetry = {
        ...restartClock,
        ...(signal === undefined ? {} : { signal }),
        onRetrying: (message) => {
          if (typeof onUpdate === "function") {
            onUpdate({ content: [{ type: "text", text: message }], details: {} });
          }
        },
      };
      try {
        const active = session(context);
        const operation = requiredString(parameters, "legion", "op");
        if (!OPERATIONS[active.kind].includes(operation)) {
          throw new Error(`${operation} is not available to a ${active.kind} session`);
        }
        if (active.kind !== "controller" && isPhaseOp(operation)) {
          const answer = await executePhaseOp(
            { daemon: daemon(), claim: active, handoffReport, branchHead, restart },
            parameters
          );
          // A call that returned without throwing: a completion, and a backward move, which hands
          // the issue to another phase. Either way this session's phase is no longer open.
          // `ready_for_update` ends no phase: it answers the pod's update task, and the worker
          // then waits for its process to be replaced rather than working, so the turn it ends
          // after answering earns no reminder (the `update-answered` input of
          // `@legion/pi-shared/phase-stall`).
          if (operation === "handoff_complete" || operation === "request_backward_move") {
            onPhaseCompleted(context);
          } else if (operation === "ready_for_update") {
            onUpdateAnswered(context);
          }
          return jsonSuccess(answer);
        }
        assertOperationInput(parameters, operation);
        const client = daemon();
        if (active.kind === "controller") {
          if (operation === "read_state") return jsonSuccess(await client.state());
          // The project's whole hold list, which names no issue: every hold is checked before
          // the grant is minted, as the daemon checks them before it redeems one, so a list with
          // one bad hold in it writes nothing. The daemon's answer is an empty object.
          if (operation === "set_merge_holds") {
            const holds = holdList(parameters);
            const { grantId } = await controllerGrant(active.sessionID);
            return jsonSuccess(await client.mergeHolds({ grantId, holds }));
          }
          // The controller's writes of one issue: each validated whole before its grant is
          // minted.
          const issue = requiredString(parameters, operation, "issue");
          if (operation === "set_merge_jump") {
            // The reason outlives the mark: the record of the jump keeps it after the merge
            // spends the mark, so a jump the daemon made names the decision behind it.
            const reason = requiredString(parameters, operation, "reason");
            const { grantId } = await controllerGrant(active.sessionID);
            return jsonSuccess(await client.mergeJump({ grantId, issue, reason }));
          }
          if (operation === "clear_merge_jump") {
            // The daemon answers the same whether a mark went or none was there: the controller
            // asked for an issue with no mark, and that is what it has.
            const { grantId } = await controllerGrant(active.sessionID);
            return jsonSuccess(await client.mergeJumpClear({ grantId, issue }));
          }
          if (operation === "fresh_session" || operation === "compact") {
            const role = LEGION_ROLES.find((candidate) => candidate === parameters.role);
            if (role === undefined) {
              throw new Error(`${operation} requires role ${LEGION_ROLES.join(", ")}`);
            }
            const { grantId } = await controllerGrant(active.sessionID);
            if (operation === "compact") {
              // The answer is the daemon's own: a compaction Oh My Pi refused or skipped says so
              // and names why, and the claim is back whatever it says.
              return jsonSuccess(await client.compact({ grantId, issue, role }));
            }
            // The daemon's answer again: empty for a claim whose phase was restarted or needed
            // no restart, and naming the refusal where the claim came back owing no task and
            // the workflow refused the restart that would have handed it one.
            return jsonSuccess(await client.freshSession({ grantId, issue, role }));
          }
          const status = ISSUE_STATUSES.find((candidate) => candidate === parameters.status);
          if (status === undefined) {
            throw new Error(`set_status requires status ${ISSUE_STATUSES.join(", ")}`);
          }
          const { grantId } = await controllerGrant(active.sessionID);
          await client.issueStatus({ grantId, issue, status });
          return jsonSuccess({});
        }
        switch (operation) {
          case "register_gate": {
            const version = parameters.version;
            if (typeof version !== "number" || !Number.isSafeInteger(version) || version < 1) {
              throw new Error("register_gate requires a positive integer version");
            }
            // The gate is the tree root's, registered by the root's own architect, as the daemon
            // requires: anyone else is refused before a lookup could answer for the wrong issue.
            const issue = requiredString(parameters, operation, "issue");
            if (active.issue !== active.tree) {
              throw new Error(
                `the design gate belongs to the tree root ${active.tree}; its root architect registers it`
              );
            }
            if (issue !== active.issue) {
              throw new Error(
                `the design gate belongs to the tree root ${active.tree}; register it there`
              );
            }
            // The daemon takes the document's id alone; `spec`, a slug or a filename, the
            // references a `dispatch` command's --artifact accepts, is looked up first, so the
            // architect's first call names the document however it knows it.
            const reference = requiredString(parameters, operation, "artifactId");
            const isId = LegionGateRegisterRequest.shape.artifactId.safeParse(reference).success;
            const artifactId = isId ? reference : await resolveDocument(issue, reference);
            // The step the document is for; a deployment that gates one step takes the
            // registration without it, and one that gates several refuses it.
            const named = LegionGateRegisterRequest.shape.step.safeParse(parameters.step);
            if (!named.success) {
              throw new Error(`register_gate's step is one of ${GATED_STEPS.join(", ")}`);
            }
            const step = named.data;
            // The fact is keyed by the gate's issue, generation, step, document, version and
            // Dispatch sequence; a repeat after the sequence moved is a re-registration the gate
            // absorbs.
            await retryWhileDaemonRestarts(
              "register_gate",
              async () =>
                client.gateRegister({
                  grantId: await mintClaimGrant(client, active),
                  issue,
                  artifactId,
                  version,
                  ...(step === undefined ? {} : { step }),
                }),
              restart
            );
            return jsonSuccess({});
          }
          case "release_children": {
            const issues = parameters.issues;
            if (!Array.isArray(issues) || !issues.every((issue) => typeof issue === "string")) {
              throw new Error("release_children requires issues");
            }
            const grantId = await mintClaimGrant(client, active);
            return jsonSuccess(await client.waveRelease({ grantId, issues }));
          }
          case "retry_or_escalate": {
            const decision = parameters.decision;
            if (decision !== "retry" && decision !== "escalate") {
              throw new Error("retry_or_escalate requires decision retry or escalate");
            }
            const grantId = await mintClaimGrant(client, active);
            await client.phaseRetry({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
              decision,
            });
            return jsonSuccess({});
          }
          case "sign_off": {
            // A repeat after a sign-off that landed finds the issue in retro and changes nothing.
            const issue = requiredString(parameters, operation, "issue");
            await retryWhileDaemonRestarts(
              "sign_off",
              async () => client.signOff({ grantId: await mintClaimGrant(client, active), issue }),
              restart
            );
            return jsonSuccess({});
          }
          case "close_root": {
            const grantId = await mintClaimGrant(client, active);
            await client.rootClose({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
              reason: requiredString(parameters, operation, "reason"),
            });
            return jsonSuccess({});
          }
          case "park_child":
          case "rerun_child": {
            const grantId = await mintClaimGrant(client, active);
            const request = { grantId, issue: requiredString(parameters, operation, "issue") };
            await (operation === "park_child"
              ? client.childPark(request)
              : client.childRerun(request));
            return jsonSuccess({});
          }
          default:
            throw new Error(`Unsupported legion operation: ${operation}`);
        }
      } catch (error) {
        return toolFailure(error);
      }
    },
  };
}
