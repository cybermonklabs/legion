import { type LegionRole, zodSchemaApi } from "@legion/contracts";
import { daemonContract, type PluginManifest } from "@legion/contracts/daemon-contract";
import {
  LEGION_PHASE_BACKWARD_TARGETS,
  LEGION_PLAN_RATING_LEAST,
  LEGION_PLAN_RATING_MOST,
  LEGION_PLAN_REASON_MAX,
  LegionInstant,
  type LegionIssue,
  type LegionPhase,
  legionPlanRatingLoneField,
  legionPlanRatingScale,
  legionPlanReasonRules,
} from "@legion/contracts/legion-api";
import { z } from "zod";
import {
  type DaemonRestartRetry,
  LegionDaemonApiError,
  type LegionDaemonClient,
  retryWhileDaemonRestarts,
} from "./daemon-client";
import type { HandoffReport } from "./handoff-record";

/** The claim a session registered, as the phase ops need it, and as the claim boot
 * (`legion-claim-core`) records it: its own id, the daemon's answer for tree, issue and role, the
 * secret every grant is minted with, and the claim token and launch generation the routes that
 * name the claim itself rather than a grant — `claims/ready`, `claims/turn-outcome`,
 * `claims/ready-for-update` — carry. The token and the secret are the pair the claim routes
 * authenticate a pod by, and they stay in this process: nothing writes either to disk. */
export interface ClaimCapability {
  readonly sessionID: string;
  readonly tree: string;
  readonly issue: string;
  readonly role: LegionRole;
  readonly secret: string;
  readonly claimToken: string;
  readonly generation: number;
  /** The task agents the registration answered with (`LegionRegistration.promptAgents`), which
   * the session's own capability measurements check its discovered agents against long after the
   * boot returned. `bootLegionClaim` is this field's only producer, and every claim in production
   * carries the list: `packages/claude-envoy/src/legion-claim.ts`, the other host that records a
   * claim, records what that boot hands it. It is optional because a reader that finds no value
   * has a defined answer — it measures against no prompt agents — so a claim built without it,
   * which only a test does, is read rather than refused. */
  readonly promptAgents?: readonly string[];
}

/**
 * A phase worker's operations on the daemon's role-local workflow surface, which every host's
 * `legion` tool serves: pi-legion's (`packages/pi-legion/src/tools.ts`, beside the architect's and
 * the controller's operations) and claude-envoy's MCP tool (`packages/claude-envoy/src/legion-tool.ts`).
 */
export const PHASE_OPS = [
  "read_record",
  "read_release_records",
  "handoff_complete",
  "request_backward_move",
  "ready_for_update",
  "read_build",
] as const;
export type PhaseOp = (typeof PHASE_OPS)[number];

export function isPhaseOp(op: string): op is PhaseOp {
  return (PHASE_OPS as readonly string[]).includes(op);
}

function namesCheck(args: Readonly<Record<string, unknown>>): boolean {
  return args["check_opened_at"] !== undefined;
}

/** A string field `op` requires: present, and not blank. The value is passed on as it was sent. */
const requiredString = (op: PhaseOp, field: string) => {
  const message = `${op} requires ${field}`;
  return z.string({ error: message }).refine((value) => value.trim() !== "", { error: message });
};

/** A string field `op` requires in the RFC 3339 form the route takes, refused here by the op's
 * own name with message rather than as the request schema's parse failure; a blank one is
 * refused as missing alone (requiredString). The form is the contract's `LegionInstant`, read
 * here so that this tool refuses exactly the strings the daemon's decode refuses:
 * `z.iso.datetime({ offset: true })` is not it, since it takes `2026-11-06T10:00Z`, which Go's
 * `time.Time` refuses, and the agent would then get the daemon's generic invalid-body answer
 * instead of the message below. */
const rfc3339String = (op: PhaseOp, field: string, message: string) =>
  requiredString(op, field).refine(
    (value) => value.trim() === "" || LegionInstant.safeParse(value).success,
    { error: message }
  );

/** `read_release_records`' window: the instant it reads from (`LegionReleaseRecordsRequest`'s
 * `since`). */
const windowStart = rfc3339String(
  "read_release_records",
  "since",
  "read_release_records' since is an RFC 3339 time, as 2026-11-02T09:00:00Z is"
);

/** `handoff_complete`'s `wake_at`: when the checking agent asks to be woken for its next look
 * (`LegionHandoffCompleteRequest`'s `wakeAt`, which is what goes on the wire). */
const nextLook = rfc3339String(
  "handoff_complete",
  "wake_at",
  "handoff_complete's wake_at is an RFC 3339 time, as 2026-11-02T09:00:00Z is"
);

/** The named production check, exactly the instant the look task supplies. */
const checkOpening = rfc3339String(
  "handoff_complete",
  "check_opened_at",
  "handoff_complete's check_opened_at is the RFC 3339 time, with seconds, its look task names"
);

/** The refusal of a `plan_rating` that is not one of the scale's whole numbers. */
const PLAN_RATING_REFUSAL = `handoff_complete's plan_rating is a whole number from ${LEGION_PLAN_RATING_LEAST} to ${LEGION_PLAN_RATING_MOST}`;

/** `handoff_complete`'s `plan_rating`: the rating of the plan the issue ran, which the agent that
 * does the retro gives in the completion that ends the retro, from 1 (the plan cost the work) to 5
 * (the plan fit the work) (`LegionHandoffCompleteRequest`'s `planRating`, which goes on the wire).
 * The scale is the contract's own (`legionPlanRatingScale`), which the contract test holds to the
 * daemon's. */
const planRating = legionPlanRatingScale(
  z.number({ error: PLAN_RATING_REFUSAL }),
  PLAN_RATING_REFUSAL
);

/** The most characters of a `plan_reason` as this tool's sentences write it: 2,000, the contract's
 * `LEGION_PLAN_REASON_MAX`. */
const PLAN_REASON_MOST = LEGION_PLAN_REASON_MAX.toLocaleString("en-US");

/** The refusal of a `plan_reason` that is missing or blank. */
const PLAN_REASON_REQUIRED = "handoff_complete requires plan_reason";

/** `handoff_complete`'s `plan_reason`: why the plan earned its rating (`planReason` on the wire). A
 * blank one is refused as missing, blank as the daemon's strings.TrimSpace reads it rather than by
 * JavaScript's trim, which the tool's other fields read (requiredString), and that refusal is its
 * only one; a reason that is not blank holds at most `LEGION_PLAN_REASON_MAX` characters, counted
 * in code points as the daemon counts them. Both are the contract's rules
 * (`legionPlanReasonRules`), read here so that this tool refuses exactly the reasons the daemon
 * refuses, by the field's name rather than as the request schema's parse failure; the daemon's
 * case golden holds the two to each other. */
const planReason = legionPlanReasonRules(z.string({ error: PLAN_REASON_REQUIRED }), {
  blank: PLAN_REASON_REQUIRED,
  long: `handoff_complete's plan_reason holds at most ${PLAN_REASON_MOST} characters`,
});

/** The refusal of a `return_to` that is not the place of a step. */
const RETURN_TO_REFUSAL =
  "handoff_complete's return_to is a whole number of 1 or more: the place of a step of your task, 1 for its first";

/** `handoff_complete`'s `return_to`: the step of the issue's running task that a step that failed
 * names for its task to return to, by its place in the task, 1 for its first
 * (`LegionHandoffCompleteRequest`'s `returnTo`, which goes on the wire). */
const returnTo = z
  .number({ error: RETURN_TO_REFUSAL })
  .int({ error: RETURN_TO_REFUSAL })
  .min(1, { error: RETURN_TO_REFUSAL });

/** The refusal of the fields `refused` that `op` does not take: each named, then once the fields
 * `op` takes (`takes`), so the agent's next call can be right. The one wording every `legion`
 * tool refuses an unknown field in, a phase op's and pi-legion's own operations' alike. */
export const unknownFieldsRefusal = (
  op: string,
  refused: readonly string[],
  takes: readonly string[]
): string =>
  [
    ...refused.map((field) => `${op} does not accept field "${field}"`),
    `it takes ${takes.length === 0 ? "none" : takes.join(", ")}`,
  ].join("; ");

/** One operation's arguments: `op` and `shape`, and nothing else. */
const operation = <Op extends PhaseOp, Shape extends z.ZodRawShape>(op: Op, shape: Shape) =>
  z.strictObject(
    { op: z.literal(op), ...shape },
    {
      error: (issue) =>
        issue.code === "unrecognized_keys"
          ? unknownFieldsRefusal(op, issue.keys, Object.keys(shape))
          : undefined,
    }
  );

/**
 * The phase ops' arguments, `op` choosing the operation: what `executePhaseOp` accepts on every
 * host, refusing a call whole before any daemon request, each problem named in the tool's error.
 */
export const PhaseOpArguments = z.discriminatedUnion(
  "op",
  [
    // The issue whose record it answers, the caller's own when the call names none: a worker
    // reads the issue it works far more often than another, and its own key is one the daemon
    // already told it (its claim), so making it say that key again only refused first calls.
    operation("read_record", {
      issue: z
        .string({ error: "read_record's issue is an issue key, as LEGION-208 is" })
        .optional(),
    }),
    operation("read_release_records", {
      since: windowStart,
    }),
    operation("handoff_complete", {
      summary: requiredString("handoff_complete", "summary"),
      // A completion is always of the caller's own issue, which the claim names, so the call
      // need not name it. One that names it anyway is taken; one that names another issue is
      // refused in executePhaseOp, before any daemon request.
      issue: z
        .string({ error: "handoff_complete's issue is an issue key, as LEGION-208 is" })
        .optional(),
      verdict: z
        .enum(["pass", "pending", "fail"], {
          error: "handoff_complete's verdict is pass, pending or fail",
        })
        .optional(),
      wake_at: nextLook.optional(),
      check_opened_at: checkOpening.optional(),
      plan_rating: planRating.optional(),
      plan_reason: planReason.optional(),
      return_to: returnTo.optional(),
    })
      // A rating of the plan carries its reason, and the daemon takes neither alone
      // (PLAN_RATING_INPUT, the contract's `legionPlanRatingLoneField`): one without the other is
      // refused with the call's other problems, naming the one missing.
      .refine(
        (args) => legionPlanRatingLoneField(args.plan_rating, args.plan_reason) !== "rating",
        {
          error:
            "handoff_complete names plan_rating without plan_reason: send the rating of the plan with one sentence of why, the two together",
        }
      )
      .refine(
        (args) => legionPlanRatingLoneField(args.plan_rating, args.plan_reason) !== "reason",
        {
          error: `handoff_complete names plan_reason without plan_rating: send the reason with the rating of the plan, a whole number from ${LEGION_PLAN_RATING_LEAST} to ${LEGION_PLAN_RATING_MOST}, the two together`,
        }
      ),
    operation("request_backward_move", {
      // The issue the move names: an architect's move names the issue it returns from accepting
      // (its grant serves its whole tree, and the daemon takes the move only on an issue that
      // architect owns); a phase worker's leaves it out, or names its own. Which is the
      // executor's to hold, by the claim's role.
      issue: z
        .string({ error: "request_backward_move's issue is an issue key, as LEGION-208 is" })
        .optional(),
      to: z.enum(LEGION_PHASE_BACKWARD_TARGETS, {
        error: (issue) =>
          typeof issue.input === "string" && issue.input.trim() !== ""
            ? `request_backward_move's to is one of ${LEGION_PHASE_BACKWARD_TARGETS.join(", ")}`
            : "request_backward_move requires to",
      }),
      reason: requiredString("request_backward_move", "reason"),
    }),
    // The signal carries one field of the agent's: the round its update message named, which the
    // daemon takes the answer for and no other. The claim says who answers, and the plugin
    // resolves the branch head it reports.
    operation("ready_for_update", { round: requiredString("ready_for_update", "round") }),
    // `read_build` takes no field: it reports this process's own build and asks the daemon for
    // the contract it states, and names no issue.
    operation("read_build", {}),
  ],
  {
    error: (issue) =>
      issue.code === "invalid_union" ? `op is one of ${PHASE_OPS.join(", ")}` : undefined,
  }
);

/** Every key a member of a union has. */
type KeysOf<T> = T extends unknown ? keyof T : never;

/** The fields the phase ops take, every op's but `op` itself. */
export type PhaseOpField = Exclude<KeysOf<z.input<typeof PhaseOpArguments>>, "op">;

/**
 * The fields of `PhaseOpArguments` as a host declares them in its `legion` tool's one object
 * schema, where `op` chooses the operation: each field once, optional, built with the host's Zod
 * (`zod`: Oh My Pi's `pi.zod`, whose converter reads only schemas its own Zod built, or
 * claude-envoy's), keyed by name so a host lays them out in the order its schema lists them. A
 * field of a kind this does not map throws, so a new field is mapped on purpose.
 */
export function phaseOpParameters(zod: unknown): Record<PhaseOpField, z.ZodType> {
  const api = zodSchemaApi(zod);
  const fields: Record<string, z.ZodType> = {};
  for (const option of PhaseOpArguments.options) {
    for (const [name, declared] of Object.entries(option.shape)) {
      if (name === "op") continue;
      const field = declared instanceof z.ZodOptional ? declared.unwrap() : declared;
      if (field instanceof z.ZodString) fields[name] = api.string().optional();
      else if (field instanceof z.ZodNumber) {
        // zod's own readings of the field's checks: whole or not, and its bounds, a side no check
        // bounds read as an infinity, which the host's number is given no bound for.
        const { isInt, minValue, maxValue } = field;
        fields[name] = api
          .number({
            int: isInt,
            ...(minValue !== null && Number.isFinite(minValue) ? { min: minValue } : {}),
            ...(maxValue !== null && Number.isFinite(maxValue) ? { max: maxValue } : {}),
          })
          .optional();
      } else if (field instanceof z.ZodBoolean) fields[name] = api.boolean().optional();
      else if (field instanceof z.ZodEnum) {
        const [first, ...rest] = field.options.map(String);
        if (first === undefined) throw new Error(`phase op field ${name} has no values`);
        fields[name] = api.enum([first, ...rest]).optional();
      } else throw new Error(`phase op field ${name} has a kind phaseOpParameters does not map`);
    }
  }
  // Built from the options' shapes, whose keys are exactly `PhaseOpField`.
  const byField = fields as Record<PhaseOpField, z.ZodType>;
  return byField;
}

/** The `legion` tool's sentences on `read_record`, `read_release_records`,
 * `request_backward_move` and `read_build`, which a host whose tool states them passes as
 * `before` and `after` (`legionToolDescription`). */
export const PHASE_OP_SENTENCES = {
  readRecord:
    "read_record returns the record of `issue` from the daemon's record route for that issue, your own issue when you leave `issue` out. " +
    "A held issue names in `heldFrom` the phase the hold took it out of, and in `holdReason` why it is held when its hold has a reason: " +
    "`escalated` (its architect sent it to the controller), `task-stopped` (its owner stopped its task with stop_task) or `children` (it " +
    "waits, before its retro, for the issues beneath it, the roots that rest on its approval or its open Dispatch children). A failed claim carries `failure`: the " +
    "`cause` word for what failed it, the `detail` sentence beside that word, `at`, when it failed, and " +
    "`attempts`, the four counts it failed with. A todo written on a held root releases the hold and starts " +
    "that phase again, as the architect's retry_or_escalate with retry does, but a root held `task-stopped` starts its task again at its " +
    "first step (a task that never started waits for its gate and its line, as any task does before its first step), and a root held " +
    "`children` stays held until none of those holds it any longer. ",
  readReleaseRecords:
    "read_release_records returns this deployment's idle-release records from `since` (an RFC 3339 time) on, read with this session's own claim: " +
    "every release, resume and hold the daemon recorded, and over them the counts of releases, of the releases among them that suspended no " +
    "role, of resumes by wake kind and of holds by reason, the pod-hours released, the share of releases woken again within ten minutes, and " +
    "the median, 90th percentile and maximum seconds from a wake to the woken role's first turn. A wake that a project pause held is " +
    "counted as `heldByPause` and left out of those seconds, since its wait measures the pause. A release that suspended no role is woken by " +
    "nothing, so the pod-hours released and the woken share count the releases that suspended a role alone. ",
  requestBackwardMove:
    "request_backward_move returns an issue to an earlier phase (`to`, `reason`). A phase worker moves its own issue. " +
    "The architect that owns an issue in accepting moves it, named by `issue`, back to the phase of an earlier step of its task. " +
    "That move counts no review round: only a review that requests changes counts one. ",
  readBuild:
    "read_build reports the build this process runs: the plugin package and version the process loaded, and the contract that build speaks. " +
    "It also reports the contract the daemon states on its answer to the health route. " +
    "That contract is null where the daemon stated none, where the daemon refused the health route and where no answer reached this process at all, and a note of its own says which. " +
    "The two numbers can differ, and the answer shows both. " +
    "Report the version this call answers, never the one at the plugin's path on the image. " +
    "In a pod, an Oh My Pi role relaunched onto the daemon's contract loads the build that relaunch installed under the role's own state home, " +
    "and a Claude Code role installs no plugin: it loads its pod image's own claude-envoy. " +
    "read_build reads no project state. ",
} as const;

/**
 * What a worker whose completion is refused HANDOFF_STALE_ENTRY does, word for word the text the
 * daemon ends that refusal with (`staleEntryInstruction` in
 * `packages/daemon/internal/api/handoff.go`), so the worker reads one instruction in its tool and
 * in the refusal. The daemon's golden test writes its constant to
 * `packages/contracts/fixtures/daemon-api/stale-entry.json`, and `legion-phase-ops.test.ts` holds
 * the description below to that file.
 */
export const STALE_ENTRY_INSTRUCTION =
  "End this turn without completing. The task of the entry the phase is on is owed to this claim, and the daemon sends it by itself. " +
  "That task is work of its own: do what it asks at the head it names and complete it with a handoff of its own, never this completion's " +
  "record, commit or verdict";

/**
 * The `legion` tool's description on every host, one template: what the tool is, then `before`,
 * then `handoff_complete`'s contract, then `after`, then where a question for another role goes,
 * `envoyPublish` being the host's name for Envoy's publish tool. `before` and `after` are whole
 * sentences, each ending in a space: the host's sentences on its other operations, and on
 * `read_record`, `read_release_records` and `request_backward_move` when it states them
 * (`PHASE_OP_SENTENCES`).
 */
export function legionToolDescription(host: {
  readonly envoyPublish: string;
  readonly before?: string;
  readonly after?: string;
}): string {
  return (
    "Perform the workflow operation the Legion daemon assigned this role. The daemon advances phases; this tool cannot spawn workers. " +
    (host.before ?? "") +
    "handoff_complete (phase workers alone: the daemon refuses an architect's completion, with ACCEPTANCE_IS_SIGN_OFF where nothing else refuses it first, since an architect accepts with sign_off; " +
    "the daemon accepts a completion only from the role working the issue's current phase, " +
    "or a role's first completion of the phase it left, which it records and which moves nothing, " +
    "and the checking role's report naming its production check with `check_opened_at`) reports this phase complete: `summary` (two sentences for the " +
    "architect), `verdict` pass|fail from the tester alone on a phase completion (the daemon refuses a verdict from " +
    "any other role there) and pass|pending|fail on a production check report (pending: production does not carry the merge " +
    "yet, or carries it with a requirement only the deploy can show still unobserved and a look still to follow; on the check's last look, only for an issue with no such requirement). " +
    "A tester's fail may name `return_to`, the place of a step of the task its issue runs, 1 for its first: the step the task goes back " +
    "to; without it, the task goes back to the first implementation step before the step that failed. Only a completion with the verdict " +
    "fail names it (RETURN_TO_NEEDS_FAIL), a place below 1 is refused (INVALID_RETURN_TO), and so is a step the task cannot go back to " +
    "(RETURN_REFUSED). " +
    "A planning, implementing, testing or reviewing phase ends with a handoff record on the " +
    "Dispatch issue: write it with `dispatch handoff-write --issue <issue> --phase <plan|implement|test|review> --body-file <file>` before " +
    "calling. This call reads from Dispatch the newest record of the phase your role works or, once the issue has moved on, of the phase it left, " +
    "and reports it with `commit`, the head that record's body names " +
    "(`head`, the 40-character lower-case commit the work it reports was done against); in implementing, the implementer's must be the commit its " +
    "workspace stands on, `@-`, pushed to `legion/<issue>@origin`, so push the branch first; retro, the merger's completion and a phase you left with no record, or with an implement record naming no head, report the commit the workspace stands on. It " +
    "refuses, posting nothing, a phase you work with no record, a Dispatch that does not answer, and, in implementing, an implement record naming no head, an " +
    "implementer's `@-` not yet on `legion/<issue>@origin`, or an implement record whose head is not that pushed `@-`, each naming the remedy. A production check report " +
    "names `check_opened_at`, the RFC 3339 opening time in its `Production check of <KEY>` task, with its stored microseconds unchanged. " +
    "Only that field selects a check report, on any phase. Without it the call completes a step. A report carries its verdict and summary, " +
    "no handoff or commit: this tool reads no record first. A pending report names `wake_at` when it asks for another look. " +
    "The daemon refuses a wake on a pass, fail or last look (CHECK_WAKE_UNEXPECTED), and a pending on any other look without one " +
    "(CHECK_WAKE_REQUIRED). It first refuses a missing, closed or differently opened check (CHECK_NOT_OPEN), then another role " +
    "(CHECK_ROLE_MISMATCH), then a look the claim has not served (CHECK_LOOK_NOT_SERVED). A report ends no phase. " +
    `The completion that ends the retro carries \`plan_rating\` (${LEGION_PLAN_RATING_LEAST} to ${LEGION_PLAN_RATING_MOST}) and \`plan_reason\`, as ` +
    "skill://legion-retro says, and the daemon refuses a completion that breaks that skill's rating rule " +
    "(PLAN_RATING_REQUIRED, PLAN_RATING_UNEXPECTED or PLAN_RATING_INPUT). " +
    "The daemon refuses " +
    "HANDOFF_SESSION_MISMATCH (another session wrote the record), HANDOFF_NOT_NEW (the record is the one you named for your previous phase: " +
    "write this phase's handoff), " +
    "HANDOFF_STALE_ENTRY (this completion is of a phase entry behind the one your role's phases row names, because the phase was entered " +
    `again while your turn ran, and nothing was recorded. ${STALE_ENTRY_INSTRUCTION}), the implementer's completion of implementing with no open ` +
    "pull request on `legion/<issue>` and none recorded " +
    "merged (HANDOFF_NO_PULL_REQUEST: open it; one naming a closed pull request is the architect's to decide); a refused completion changed " +
    "nothing, so fix what it names and complete again. " +
    "ready_for_update answers the daemon's update message on this issue's pod: `round` is the round id that message names, and the " +
    "daemon takes your answer for that round alone. The reviewer creates no commit and pushes nothing, so each commit " +
    "and push this description names is not its own, and the merger pushes the merge of the base it made. Push every commit you want kept to the issue branch " +
    "first. Then let each subagent and each background job that you started finish, or stop it. Commit and push what it made " +
    "before you answer. The daemon replaces the pod and its disk when the round ends. A kept volume stays with its Sandbox " +
    "through the round, but a reset or a change of its disk shape still deletes it, so push first there too. If the daemon " +
    "finds work that origin does not hold, it keeps the pod and says so; push that work. A round over a drift that breaks the " +
    "pod replaces the pod anyway. The round ends at the moment the message " +
    "names. It can end earlier, once every role has answered, no role is in a turn and each role's last report names no running " +
    "subagent or background job. No answer delays that moment, and the end of a turn is no answer. If a subagent or a background " +
    "job finishes after you answer, commit and push what it made. Then call ready_for_update again with the same round. " +
    "The call ends no phase. The daemon holds your next task until your process comes back on the new pod, in this same session. " +
    (host.after ?? "") +
    `What a later phase needs goes in your handoff; a question for another live role goes to its role topic with ${host.envoyPublish}.`
  );
}

/** The plugin build one process loaded, as its host's own `package.json` names it: the package
 * `name`, its `version`, and the daemon API contract that build speaks (`daemonContract`: the
 * `legion.daemonApiVersion` a pack writes, which the client states on every request as
 * `pluginContract` and `read_build` answers beside the daemon's own). It is the host's manifest
 * and never a path on the image: an Oh My Pi role's relaunch onto the daemon's contract installs
 * the plugin pair fresh under the role's state home, so the build at `/opt/legion/pi-legion` is
 * another one. A Claude Code role in a pod installs no plugin, and its build is its image's own
 * claude-envoy. */
export interface PluginBuild {
  readonly name: string;
  readonly version: string;
  readonly pluginContract: number;
}

/** The three values of `PluginBuild` read off one host's manifest. `pi-shared` imports no
 * manifest and is bundled into both plugins, so each host calls this with the `package.json` it
 * imported and its own directory, the `from` of `daemonContract`
 * (`packages/pi-legion/src/tools.ts`, `packages/claude-envoy/src/legion-tool.ts`). It throws
 * where `daemonContract` does: a manifest that names no contract outside a checkout. */
export function pluginBuildOf(
  manifest: PluginManifest & { readonly name: string; readonly version: string },
  from: string
): PluginBuild {
  return {
    name: manifest.name,
    version: manifest.version,
    pluginContract: daemonContract(manifest, from),
  };
}

export interface PhaseOpRuntime {
  readonly daemon: LegionDaemonClient;
  readonly claim: ClaimCapability;
  /** The build this process loaded, which `read_build` answers with (`pluginBuildOf`). */
  readonly build: PluginBuild;
  /** What a completion by `role` reports beside its summary while the issue stands at `phase`
   * (`findHandoffReport` with the pane's environment and Dispatch): the newest record of the
   * phase the role works or, once the issue has moved on, of the phase it left, or `@-` alone where
   * that left phase has none or the implementer's left record names no head. Throws naming the remedy when the phase the role works has no
   * handoff record, Dispatch does not answer, or, while the issue stands in implementing, the
   * implementer's record names no head, the commit its workspace stands on is not pushed, or its
   * record's head is not that pushed commit. */
  readonly handoffReport: (phase: LegionPhase, role: LegionRole) => Promise<HandoffReport>;
  /** The head of `legion/<issue>@origin` the `ready_for_update` signal reports
   * (`findPushedBranchHead` with the pane's environment): `""` when the branch is not there. A
   * throw leaves the head out of the signal, which is still sent, and the throw's message goes
   * to the agent in the answer's `note`. */
  readonly branchHead: () => Promise<string>;
  /** How `handoff_complete` waits out a daemon restart (`retryWhileDaemonRestarts`): the host's
   * sentence to the agent, and a test's clock. */
  readonly restart?: DaemonRestartRetry;
  /** The clock `read_release_records` measures a release the daemon has not resumed yet against,
   * `Date` itself unless a test hands it another. */
  readonly now?: () => Date;
}

/** A grant for one write of the claim's session, minted on `/grants`' session form with the
 * claim's secret: every write of the `legion` tool posts a fresh one with its request. */
export async function mintClaimGrant(
  daemon: LegionDaemonClient,
  claim: ClaimCapability
): Promise<string> {
  const { grantId } = await daemon.grant({
    sessionId: claim.sessionID,
    secret: claim.secret,
    tree: claim.tree,
    issue: claim.issue,
  });
  return grantId;
}

/** One issue's record, read from the record route (`LegionDaemonClient.record`), which answers
 * about two kilobytes where the whole-project body answers megabytes. The words for an issue
 * the daemon records nothing of are the ones `read_record` answered while it read the project.
 * Every other refusal of the route throws as it came. */
async function recordFrom(daemon: LegionDaemonClient, issue: string): Promise<LegionIssue> {
  const record = await daemon.record(issue);
  if (record === undefined) throw new Error(`The daemon has no record for ${issue}`);
  return record;
}

const ROOT_COMPLETION_REFUSAL =
  "handoff_complete is not available to a root architect session, which runs no phase";

/** The three words `read_build` answers under `daemonContractNote`, where `daemonContract` is
 * null: the daemon answered the health route and stated no contract, which a daemon older than
 * the header does; the daemon answered the route and refused it; or no answer reached this
 * process at all, which is what a daemon that is down or unreachable leaves. */
const DAEMON_STATED_NO_CONTRACT = "the daemon stated no contract";
const DAEMON_REFUSED_ROUTE = "the daemon refused the health route";
const DAEMON_DID_NOT_ANSWER = "the daemon did not answer at all";

/** The contract the daemon states on its answer to the health route, with the word for an
 * answer that carries no number. `daemonContract` throws on a refusal and on the transport
 * failure of a daemon that is down, which is the moment a role most needs to know which build
 * it runs, so both are answered as the note, and the note says which of the two happened: the
 * package name, the version and this build's own contract need no request and are reported
 * whatever the daemon does. */
async function statedDaemonContract(
  daemon: LegionDaemonClient
): Promise<{ readonly contract: number | null; readonly note: string | null }> {
  let stated: number | undefined;
  try {
    stated = await daemon.daemonContract();
  } catch (error) {
    // The route is read without a schema, so the client raises no contract error for it: the
    // daemon answered and refused (`LegionDaemonApiError`, which carries the status), or the
    // request reached no answer at all and the transport threw.
    return {
      contract: null,
      note: error instanceof LegionDaemonApiError ? DAEMON_REFUSED_ROUTE : DAEMON_DID_NOT_ANSWER,
    };
  }
  return stated === undefined
    ? { contract: null, note: DAEMON_STATED_NO_CONTRACT }
    : { contract: stated, note: null };
}

/** `read_build`: the package name, the version and the contract of the build this process
 * loaded, and the contract the daemon states on its answer to the health route, null where it
 * states none or does not answer, with `daemonContractNote` saying which. The three local
 * values need no request. Every claim session reaches it through `executePhaseOp`, and the
 * controller's session, which holds no claim, calls it here
 * (`packages/pi-legion/src/tools.ts`). */
export async function readBuild(runtime: {
  readonly daemon: LegionDaemonClient;
  readonly build: PluginBuild;
}): Promise<Readonly<Record<string, unknown>>> {
  const stated = await statedDaemonContract(runtime.daemon);
  return {
    plugin: runtime.build.name,
    version: runtime.build.version,
    pluginContract: runtime.build.pluginContract,
    daemonContract: stated.contract,
    daemonContractNote: stated.note,
  };
}

/**
 * Runs one phase op and answers its JSON result: `{ record }` for `read_record`, of the issue it
 * names or of the claim's own when it names none, the window's records and their counts for
 * `read_release_records`, the daemon's answer for `handoff_complete`, `{}` for
 * `request_backward_move`, this process's build and the daemon's stated contract for
 * `read_build`, and `{}` for `ready_for_update`, or `{ note }` when its branch-head lookup
 * failed. A refusal throws before any write, minting nothing and posting nothing: each problem
 * `PhaseOpArguments` finds, joined by `; `, a field the operation does not take named first,
 * then a root architect's completion,
 * then each problem with the operation's own fields, a lone plan rating or reason, an invalid
 * verdict, a phase call's pending or wake_at without check_opened_at, a named
 * report's return_to, the issue a completion or move names, or a refused handoff report.
 * A daemon refusal throws its code and sentence. Only check_opened_at selects a look report:
 * it goes straight to the daemon with no record or commit lookup. A step completion reads the
 * issue's phase to find its handoff, never to decide the call's meaning. The daemon decides
 * check state, role, served look and last-look wake rules; a plan rating is sent as given.
 */
export async function executePhaseOp(
  runtime: PhaseOpRuntime,
  input: unknown
): Promise<Readonly<Record<string, unknown>>> {
  const { daemon, claim } = runtime;
  const parsed = PhaseOpArguments.safeParse(input);
  const issues = parsed.success ? [] : parsed.error.issues;
  const unknownFields = issues.filter((issue) => issue.code === "unrecognized_keys");
  const fieldProblems = issues.filter((issue) => issue.code !== "unrecognized_keys");
  const rootCompletion =
    typeof input === "object" &&
    input !== null &&
    "op" in input &&
    input.op === "handoff_complete" &&
    claim.role === "architect" &&
    claim.issue === claim.tree;
  if (rootCompletion) {
    throw new Error(
      [...unknownFields.map((issue) => issue.message), ROOT_COMPLETION_REFUSAL].join("; ")
    );
  }
  if (!parsed.success) {
    throw new Error([...unknownFields, ...fieldProblems].map((issue) => issue.message).join("; "));
  }
  const args = parsed.data;
  switch (args.op) {
    case "read_record": {
      const named = args.issue?.trim() ?? "";
      const issue = named !== "" ? named : claim.issue;
      return { record: await recordFrom(daemon, issue) };
    }
    case "read_release_records":
      return releaseRecords(runtime, args.since);
    case "handoff_complete": {
      // The claim names the one issue a completion reports, so an `issue` in the call is only
      // checked against it and never sent: one naming another issue is refused here, before the
      // record is read or a grant minted.
      const named = args.issue?.trim() ?? "";
      if (named !== "" && named !== claim.issue) {
        throw new Error(
          `handoff_complete reports this session's own issue ${claim.issue}; leave issue out, or name ${claim.issue}, not ${named}`
        );
      }
      // Only a step that failed names the step its task returns to, and the daemon refuses one on
      // any other completion (RETURN_TO_NEEDS_FAIL), so it is refused here in the same words.
      if (args.return_to !== undefined && args.verdict !== "fail") {
        throw new Error(
          "handoff_complete names return_to without the verdict fail: only the completion of a step that failed, with the verdict fail, names the step its task returns to; send it again without return_to"
        );
      }
      // The rating of the plan is built here, where the call was parsed: `PhaseOpArguments`' pair
      // refines hold the two fields together, so this is where the two become the one value the
      // completion carries, under the daemon's names, or nothing.
      const { plan_rating: planRating, plan_reason: planReason } = args;
      const rating =
        planRating === undefined || planReason === undefined
          ? undefined
          : { planRating, planReason };
      // Retried whole while the daemon restarts: the record, the handoff report and a fresh grant
      // are read again on each attempt. A repeat never applies twice, but it cannot tell a landed
      // attempt from a refused one (the daemon records both under the completion's key), so its
      // refusal is passed on, marked as possibly following an applied attempt.
      return retryWhileDaemonRestarts(
        "handoff_complete",
        () => completeHandoff(runtime, args, rating),
        runtime.restart
      );
    }
    case "request_backward_move": {
      // An architect's grant serves its whole tree, so its move names the issue it returns from
      // accepting. A phase worker's grant names its own issue, the only one it may move, so its
      // request leaves `issue` out; one naming another issue is refused here, as the daemon would
      // (ISSUE_NOT_GRANTS).
      let issue: string | undefined;
      if (claim.role === "architect") {
        const named = args.issue?.trim() ?? "";
        if (named === "") {
          throw new Error("request_backward_move requires issue");
        }
        issue = named;
      } else {
        const named = args.issue?.trim() ?? "";
        if (named !== "" && named !== claim.issue) {
          throw new Error(`a phase worker moves only its own issue ${claim.issue}, not ${named}`);
        }
      }
      await daemon.phaseBackward({
        grantId: await mintClaimGrant(daemon, claim),
        to: args.to,
        reason: args.reason,
        ...(issue === undefined ? {} : { issue }),
      });
      return {};
    }
    case "ready_for_update": {
      // The claim authenticates the signal, as it does `claims/ready`: no grant is minted. The
      // round is the agent's own word, read from the update message it answers, and the daemon
      // takes the answer for that round alone — an answer for a round its claim has left is
      // refused naming both. The head is the plugin's to resolve, and a branch not yet on the
      // remote is left out. A workspace whose jj cannot answer leaves it out too, and is told so:
      // the round ends before its bound only once every claim of the pod has answered, no turn
      // runs and no claim's activity report holds it (the daemon's `roundEnd.holding`), so a
      // worker whose lookup fails must still be able to answer. The daemon verifies no head, so
      // the missing one costs the record a line and the round nothing.
      let head = "";
      let failure: string | undefined;
      try {
        head = await runtime.branchHead();
      } catch (error) {
        failure = error instanceof Error ? error.message : String(error);
      }
      await daemon.readyForUpdate({
        claimToken: claim.claimToken,
        sessionId: claim.sessionID,
        secret: claim.secret,
        generation: claim.generation,
        round: args.round,
        ...(head === "" ? {} : { head }),
      });
      // The answer names the signal the daemon has and the head it misses, and carries the
      // lookup's own message, so the worker can repair what it names.
      return failure === undefined
        ? {}
        : {
            note:
              "The daemon has your ready_for_update signal. The signal reports no branch head, " +
              "because the branch-head lookup failed. The daemon checks no head, so your signal " +
              "counts without one. Repair what the lookup's message names before your next jj " +
              `command. The lookup said: ${failure}`,
          };
    }
    case "read_build":
      // The one read of this operation, and it asks for no project state: the health route,
      // read for its `Legion-Daemon-Contract` header alone (`LegionDaemonClient.daemonContract`
      // parses no body, so a daemon whose health body this build cannot read still answers the
      // number). The build is the host's own manifest, passed in at the tool.
      return readBuild(runtime);
  }
}

/** A release woken again within this of its release is churn: the pod was given up and paid for
 * again at once (`docs/kubernetes.md`, "Idle release"). */
const WOKEN_SOON_MS = 10 * 60 * 1000;

/** How many rows of each `key`, the key's own order kept. */
const countedBy = <Row>(
  rows: readonly Row[],
  key: (row: Row) => string
): Readonly<Record<string, number>> => {
  const counts: Record<string, number> = {};
  for (const row of rows) counts[key(row)] = (counts[key(row)] ?? 0) + 1;
  return counts;
};

/** The nearest-rank percentile of `sorted`, ascending: the smallest value `share` of the rows
 * are at or below. Null for no rows, which is a window nothing was measured in rather than a
 * zero. */
const percentile = (sorted: readonly number[], share: number): number | null =>
  sorted.length === 0 ? null : (sorted[Math.max(Math.ceil(share * sorted.length), 1) - 1] ?? null);

/**
 * `read_release_records`: the window's records from the daemon, with the numbers a report of the
 * deployment's idle release is written from, so an agent counts nothing itself. The claim's own
 * token and secret authenticate the read, as every other claim route is authenticated, and they
 * never leave this process.
 *
 * A release that suspended no role is known by the empty `roles` list the record already carries:
 * every release of the rule that reclaims an issue pod no claim can use is one, and so is the
 * rare idle release whose stops suspended nobody. No role of it is ever woken, so nothing records
 * when its issue's pod came back, and the three measurements of a release's life leave it out.
 * `counts.releases` counts every release and `counts.releasesSuspendingNoRole` counts these.
 *
 * `podHoursReleased` is how long the released pods were gone: from each release that suspended a
 * role to the first wake of its pod, and to `asOf` for one nothing has woken yet, since that pod
 * is gone now. A pod's roles wake one resume each, so the first of them ends the release. A
 * resume names the release it ended, except a wake a project pause held, which names none
 * (`release` 0, `PodRecords.TakePodWake` in `packages/daemon/internal/store/wake_take.go`). A held
 * wake therefore ends every release of its issue that came before it, where no resume that names the
 * release came sooner: a pod released before a pause and woken during it would otherwise be
 * charged to `asOf` at every read, for ever. The release ends at the wake's own moment, which is
 * when the pod was wanted again. The pod stays gone from there until the project runs again, and
 * that time is the pause's, as the wake's seconds to its first turn are. A release that
 * suspended no role is charged nothing, because charging it that way would bill the fleet from
 * its moment to the moment of every later read, for ever, though an operator's retry may have
 * rebuilt the pod a minute later.
 * `wokenWithinTenMinutes` counts the releases a wake followed that soon, a held wake as any
 * other, the churn of releasing a pod that was wanted again at once, over the releases that
 * suspended a role alone: a release no wake can reach belongs in neither half of that share,
 * which it would otherwise lower for ever. `wakeToFirstTurnSeconds` measures each resume no pause
 * held that the daemon has seen a first turn for, `withoutFirstTurn` counting the rest of the
 * resumes no pause held, and reports the nearest-rank median and 90th percentile and the
 * maximum; it reads the resume rows, of which a release that suspended no role has none, so it
 * leaves those releases out by its own shape. A resume that carries `pause` is a wake a project
 * pause held (`api.ResumeRecord`): it waited until the project ran again, so its seconds to the
 * first turn would measure the pause and not the pod's return. `heldByPause` counts those
 * resumes, and they are in neither `measured` nor `withoutFirstTurn`.
 */
async function releaseRecords(
  runtime: PhaseOpRuntime,
  since: string
): Promise<Readonly<Record<string, unknown>>> {
  const { claim } = runtime;
  const records = await runtime.daemon.releaseRecords({
    claimToken: claim.claimToken,
    secret: claim.secret,
    since,
  });
  const asOf = runtime.now?.() ?? new Date();
  const wokenAt = new Map<number, number>();
  const heldWakes = new Map<string, number[]>();
  for (const resume of records.resumes) {
    const at = Date.parse(resume.at);
    // A wake a pause held names no release, so it is kept under its issue for the releases of
    // that issue below.
    if (resume.pause !== undefined) {
      const held = heldWakes.get(resume.issue);
      if (held === undefined) heldWakes.set(resume.issue, [at]);
      else held.push(at);
      continue;
    }
    if (resume.release === 0) continue;
    const first = wokenAt.get(resume.release);
    if (first === undefined || at < first) wokenAt.set(resume.release, at);
  }
  let releasedMs = 0;
  let wokenSoon = 0;
  let suspendedNoRole = 0;
  for (const release of records.releases) {
    // The empty role list is the whole mark of a release no wake can reach, so it is counted
    // here and measured nowhere.
    if (release.roles.length === 0) {
      suspendedNoRole += 1;
      continue;
    }
    const from = Date.parse(release.at);
    let woken = wokenAt.get(release.id);
    for (const at of heldWakes.get(release.issue) ?? []) {
      if (at >= from && (woken === undefined || at < woken)) woken = at;
    }
    releasedMs += Math.max((woken ?? asOf.getTime()) - from, 0);
    if (woken !== undefined && woken - from <= WOKEN_SOON_MS) wokenSoon += 1;
  }
  const wakeable = records.releases.length - suspendedNoRole;
  let heldByPause = 0;
  let withoutFirstTurn = 0;
  const waits: number[] = [];
  for (const resume of records.resumes) {
    // A wake a project pause held is counted here and measured nowhere, whether or not its
    // first turn has come: its wait is the pause's.
    if (resume.pause !== undefined) heldByPause += 1;
    else if (resume.firstTurnAt === undefined) withoutFirstTurn += 1;
    else waits.push((Date.parse(resume.firstTurnAt) - Date.parse(resume.at)) / 1000);
  }
  waits.sort((one, other) => one - other);
  const whole = (value: number | null) => (value === null ? null : Math.round(value));
  return {
    since,
    asOf: asOf.toISOString(),
    releases: records.releases,
    resumes: records.resumes,
    holds: records.holds,
    counts: {
      releases: records.releases.length,
      releasesSuspendingNoRole: suspendedNoRole,
      resumes: records.resumes.length,
      resumesByKind: countedBy(records.resumes, (resume) => resume.kind),
      holds: records.holds.length,
      holdsByReason: countedBy(records.holds, (hold) => hold.reason),
    },
    podHoursReleased: Math.round((releasedMs / 3_600_000) * 100) / 100,
    wokenWithinTenMinutes: {
      releases: wokenSoon,
      share: wakeable === 0 ? 0 : Math.round((wokenSoon / wakeable) * 1000) / 1000,
    },
    wakeToFirstTurnSeconds: {
      measured: waits.length,
      withoutFirstTurn,
      heldByPause,
      median: whole(percentile(waits, 0.5)),
      p90: whole(percentile(waits, 0.9)),
      max: whole(waits.at(-1) ?? null),
    },
  };
}

/** The rating of the plan a completion carries, under the daemon's names: the two together, which
 * `PhaseOpArguments` takes only together. */
interface PlanRatingGiven {
  readonly planRating: number;
  readonly planReason: string;
}

/** One attempt of `handoff_complete` (`executePhaseOp`'s doc comment), with the rating of the plan
 * the call gave, or none. */
async function completeHandoff(
  runtime: PhaseOpRuntime,
  args: Extract<z.infer<typeof PhaseOpArguments>, { op: "handoff_complete" }>,
  rating: PlanRatingGiven | undefined
): Promise<Readonly<Record<string, unknown>>> {
  const { daemon, claim } = runtime;
  // Only the explicit field selects a look report. No record read chooses the call or adds a
  // handoff to it; the daemon checks the named check, its role and its last-look state.
  if (namesCheck(args)) {
    if (args.verdict === undefined) {
      throw new Error(
        "handoff_complete with check_opened_at names a verdict of pass, pending or fail"
      );
    }
    if (args.return_to !== undefined) {
      throw new Error(
        "handoff_complete's return_to names the step a failed step's task returns to; a report with check_opened_at names none"
      );
    }
    if (args.wake_at !== undefined && args.verdict !== "pending") {
      throw new Error(
        "handoff_complete names wake_at and no look follows a pass or fail; send it again without a time"
      );
    }
    return daemon.handoffComplete({
      grantId: await mintClaimGrant(daemon, claim),
      summary: args.summary,
      verdict: args.verdict,
      checkOpenedAt: args.check_opened_at,
      ...(args.wake_at === undefined ? {} : { wakeAt: args.wake_at }),
      ...rating,
    });
  }
  if (args.verdict === "pending") {
    throw new Error(
      "handoff_complete's pending verdict reports a production check. Send check_opened_at with the time from the look task or check.openedAt from read_record. To complete a step, send pass or fail."
    );
  }
  // The daemon refuses a phase completion that carries a wake (CHECK_WAKE_UNEXPECTED); this says
  // so without the round trip.
  if (args.wake_at !== undefined) {
    throw new Error(
      "handoff_complete's wake_at reports a production check. Send check_opened_at with the time from the look task or check.openedAt from read_record. A step's completion carries no wake."
    );
  }
  // The record and the commit are found before the grant is minted. The phase is the issue
  // record's, the role the claim's.
  const record = await recordFrom(daemon, claim.issue);
  const report = await runtime.handoffReport(record.phase, claim.role);
  return daemon.handoffComplete({
    grantId: await mintClaimGrant(daemon, claim),
    summary: args.summary,
    verdict: args.verdict ?? "",
    ...report,
    ...rating,
    ...(args.return_to === undefined ? {} : { returnTo: args.return_to }),
  });
}
