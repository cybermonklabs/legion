/**
 * The `legion` tool's declarations every host serves: the phase ops' roster, their argument
 * schemas and the conversion of those schemas to a host's own Zod, and the tool's description.
 * The one executor of the phase ops is `executePhaseOp` (`./legion-phase-ops`), which parses each
 * call with `PhaseOpArguments` and refuses it before any daemon request.
 */
import { type LegionRole, zodSchemaApi } from "@legion/contracts";
import {
  LEGION_PHASE_BACKWARD_TARGETS,
  LEGION_PLAN_RATING_LEAST,
  LEGION_PLAN_RATING_MOST,
  LEGION_PLAN_REASON_MAX,
  LEGION_REVIEWER_VERDICTS,
  LegionInstant,
  legionPlanRatingLoneField,
  legionPlanRatingScale,
  legionPlanReasonRules,
} from "@legion/contracts/legion-api";
import { z } from "zod";

/**
 * A phase worker's operations on the daemon's role-local workflow surface, which every host's
 * `legion` tool serves: pi-legion's (`packages/pi-legion/src/tools.ts`, beside the architect's and
 * the controller's operations) and claude-envoy's MCP tool (`packages/claude-envoy/src/legion-tool.ts`).
 * `review_verdict` is a review step's agent's alone (`REVIEW_STEP_ROLES`): each host offers a
 * claim's session the phase ops of its role (`phaseOpsOf`), and `executePhaseOp` holds it for
 * every host.
 */
export const PHASE_OPS = [
  "read_record",
  "read_release_records",
  "handoff_complete",
  "request_backward_move",
  "ready_for_update",
  "review_verdict",
  "read_build",
] as const;
export type PhaseOp = (typeof PHASE_OPS)[number];

export function isPhaseOp(op: string): op is PhaseOp {
  return (PHASE_OPS as readonly string[]).includes(op);
}

/** The roles whose claim runs a step of the type review, the daemon's `plan.BuiltinRoles`: the
 * reviewer runs the step `reviewer`, and the architect that owns an issue runs its step
 * `acceptance:owner` (`workflow/steps.go`, `acceptingAgent`), so the owner's acceptance session is
 * an architect's. A role a plan defines runs no step in this release (`plan.Supported`).
 * `review_verdict` is theirs alone on every host. */
export const REVIEW_STEP_ROLES: readonly LegionRole[] = ["reviewer", "architect"];

/** The phase ops a claim of `role` runs, which a host offers that claim's session: every one for
 * a role whose claim runs a review step (`REVIEW_STEP_ROLES`), and every one but `review_verdict`
 * for each other role. */
export function phaseOpsOf(role: LegionRole): readonly PhaseOp[] {
  return REVIEW_STEP_ROLES.includes(role)
    ? PHASE_OPS
    : PHASE_OPS.filter((op) => op !== "review_verdict");
}

/** A string field `op` requires: present, and not blank. The value is passed on as it was sent. */
const requiredString = (op: PhaseOp, field: string) => {
  const message = `${op} requires ${field}`;
  return z.string({ error: message }).refine((value) => value.trim() !== "", { error: message });
};

/** The issue key `op` may name, as LEGION-208 is: optional, and which issue it may be is the
 * executor's to hold. */
const optionalIssue = (op: PhaseOp) =>
  z.string({ error: `${op}'s issue is an issue key, as LEGION-208 is` }).optional();

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

/** `handoff_complete`'s `return_to`: the step of the issue's running task that a tester's
 * completion names for its task to return to, should the check states at the head it tested fail
 * its step, by its place in the task, 1 for its first (`LegionHandoffCompleteRequest`'s
 * `returnTo`, which goes on the wire). */
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
      issue: optionalIssue("read_record"),
    }),
    operation("read_release_records", {
      since: windowStart,
    }),
    operation("handoff_complete", {
      summary: requiredString("handoff_complete", "summary"),
      // A completion is always of the caller's own issue, which the claim names, so the call
      // need not name it. One that names it anyway is taken; one that names another issue is
      // refused in executePhaseOp, before any daemon request.
      issue: optionalIssue("handoff_complete"),
      // The verdict of a review step's agent on its phase completion, approve or changes, which
      // the reviewer sends on its completion of reviewing (the owner accepts with sign_off and
      // completes nothing), and pass, pending or fail on a production check's report. No other
      // phase completion carries one: Dispatch's check states at the tested head decide the
      // testing phase, and executePhaseOp refuses any other role's verdict once the record says
      // the completion is a phase's, as the daemon does (VERDICT_NOT_TAKEN, VERDICT_ROLE_FORBIDDEN).
      // Which word the reviewer sends is the daemon's to hold (REVIEWER_VERDICT_REQUIRED).
      verdict: z
        .enum(["pass", "pending", "fail", ...LEGION_REVIEWER_VERDICTS], {
          error: "handoff_complete's verdict is pass, pending, fail, approve or changes",
        })
        .optional(),
      wake_at: nextLook.optional(),
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
      issue: optionalIssue("request_backward_move"),
      to: z.enum(LEGION_PHASE_BACKWARD_TARGETS, {
        error: (issue) =>
          typeof issue.input === "string" && issue.input.trim() !== ""
            ? `request_backward_move's to is one of ${LEGION_PHASE_BACKWARD_TARGETS.join(", ")}`
            : "request_backward_move requires to",
      }),
      reason: requiredString("request_backward_move", "reason"),
      // The head an architect sends back from accepting, its acceptance's changes there, which the
      // daemon takes only at the pull request's current head (VERDICT_HEAD_NOT_CURRENT). A phase
      // worker's move is no verdict and names none. Which is the executor's to hold, by the role.
      // It gives no description of its own: a host declares `commit` once, in review_verdict's
      // words (phaseOpParameters), and the move's sentence says what it names
      // (PHASE_OP_SENTENCES.requestBackwardMove).
      commit: z
        .string({ error: "request_backward_move's commit is the head you send back" })
        .optional(),
    }),
    // The signal carries one field of the agent's: the round its update message named, which the
    // daemon takes the answer for and no other. The claim says who answers, and the plugin
    // resolves the branch head it reports.
    operation("ready_for_update", { round: requiredString("ready_for_update", "round") }),
    // The verdict of a review step's agent on a head its pull request moved to past review
    // (`LegionReviewVerdictRequest`): the issue an architect accepted, its two words, the head and
    // its summary. Who may send it, and which issue it names, is the executor's to hold, by the
    // claim's role.
    operation("review_verdict", {
      issue: optionalIssue("review_verdict"),
      verdict: z.enum(LEGION_REVIEWER_VERDICTS, {
        error: `review_verdict requires verdict ${LEGION_REVIEWER_VERDICTS.join(" or ")}`,
      }),
      commit: requiredString("review_verdict", "commit").describe(
        "review_verdict's head: the pull request's current head, the commit your verdict is on"
      ),
      summary: requiredString("review_verdict", "summary"),
    }),
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
 * claude-envoy's), keyed by name so a host lays them out in the order its schema lists them, and
 * described as its op describes it. A field more than one op takes is one field: a string stays a
 * string, and an enum takes every value any of those ops takes (`verdict` is a completion's five
 * words, two of which are review_verdict's). A field of a kind this does not map throws, and so do
 * a field two ops declare as two kinds, a field of another kind two ops take, and a field two ops
 * describe two ways, so a new field is mapped on purpose.
 */
export function phaseOpParameters(zod: unknown): Record<PhaseOpField, z.ZodType> {
  const api = zodSchemaApi(zod);
  const declarations = new Map<string, { field: z.ZodType; description: string | undefined }[]>();
  for (const option of PhaseOpArguments.options) {
    for (const [name, declared] of Object.entries(option.shape)) {
      if (name === "op") continue;
      const field = declared instanceof z.ZodOptional ? declared.unwrap() : declared;
      const declaration = { field, description: declared.description ?? field.description };
      const seen = declarations.get(name);
      if (seen === undefined) declarations.set(name, [declaration]);
      else seen.push(declaration);
    }
  }
  const fields: Record<string, z.ZodType> = {};
  for (const [name, declaredBy] of declarations) {
    const declared = declaredBy.map((each) => each.field);
    const [field] = declared;
    if (declared.some((other) => other.constructor !== field?.constructor)) {
      throw new Error(`phase op field ${name} is of two kinds in two ops`);
    }
    if (declared.length > 1 && !(field instanceof z.ZodString || field instanceof z.ZodEnum)) {
      throw new Error(
        `phase op field ${name} is taken by two ops, and only a string or an enum is`
      );
    }
    let node: z.ZodType;
    if (field instanceof z.ZodString) node = api.string();
    else if (field instanceof z.ZodNumber) {
      // zod's own readings of the field's checks: whole or not, and its bounds, a side no check
      // bounds read as an infinity, which the host's number is given no bound for.
      const { isInt, minValue, maxValue } = field;
      node = api.number({
        int: isInt,
        ...(minValue !== null && Number.isFinite(minValue) ? { min: minValue } : {}),
        ...(maxValue !== null && Number.isFinite(maxValue) ? { max: maxValue } : {}),
      });
    } else if (field instanceof z.ZodBoolean) node = api.boolean();
    else if (field instanceof z.ZodEnum) {
      const values = declared.flatMap((each) =>
        each instanceof z.ZodEnum ? each.options.map(String) : []
      );
      const [first, ...rest] = [...new Set(values)];
      if (first === undefined) throw new Error(`phase op field ${name} has no values`);
      node = api.enum([first, ...rest]);
    } else throw new Error(`phase op field ${name} has a kind phaseOpParameters does not map`);
    const descriptions = new Set(declaredBy.flatMap((each) => each.description ?? []));
    const [description, ...others] = [...descriptions];
    if (others.length > 0)
      throw new Error(`phase op field ${name} is described two ways in two ops`);
    fields[name] = (description === undefined ? node : node.describe(description)).optional();
  }
  // Built from the options' shapes, whose keys are exactly `PhaseOpField`.
  const byField = fields as Record<PhaseOpField, z.ZodType>;
  return byField;
}

/** The `legion` tool's sentences on `read_record`, `read_release_records`,
 * `request_backward_move`, `review_verdict` and `read_build`, which a host whose tool states them passes as
 * `before` and `after` (`legionToolDescription`). */
export const PHASE_OP_SENTENCES = {
  readRecord:
    "read_record returns the record of `issue` from the daemon's record route for that issue, your own issue when you leave `issue` out. " +
    "A held issue names in `heldFrom` the phase the hold took it out of, and in `holdReason` why it is held when its hold has a reason: " +
    "`escalated` (its architect sent it to the controller), `task-stopped` (its owner stopped its task with stop_task) or `children` (it " +
    "waits, before its retro, for the issues beneath it). A failed claim carries `failure`: the " +
    "`cause` word for what failed it, the `detail` sentence beside that word, `at`, when it failed, and " +
    "`attempts`, the four counts it failed with. A todo written on a held root releases the hold and starts " +
    "that phase again, as the architect's retry_or_escalate with retry does, but a root held `task-stopped` starts its task again at its " +
    "first step (a task that never started waits for its gate and its line, as any task does before its first step), and a root held " +
    "`children` stays held until the issues beneath it close. ",
  readReleaseRecords:
    "read_release_records returns this deployment's idle-release records from `since` (an RFC 3339 time) on, read with this session's own claim: " +
    "every release, resume and hold the daemon recorded, and over them the counts of releases, of the releases among them that suspended no " +
    "role, of resumes by wake kind and of holds by reason, the pod-hours released, the share of releases woken again within ten minutes, and " +
    "the median, 90th percentile and maximum seconds from a wake to the woken role's first turn. A wake that a project pause held is " +
    "counted as `heldByPause` and left out of those seconds, since its wait measures the pause. A release that suspended no role is woken by " +
    "nothing, so the pod-hours released and the woken share count the releases that suspended a role alone. ",
  requestBackwardMove:
    "request_backward_move returns an issue to an earlier phase (`to`, `reason`). A phase worker moves its own issue. " +
    "The daemon refuses the tester's move out of testing (BACKWARD_REFUSED), since a testing step ends by its checks' states: the tester " +
    "records each check's result with `dispatch result`, a fail with its evidence for a check it cannot verify, and completes. " +
    "The architect that owns an issue in accepting moves it, named by `issue`, back to the phase of an earlier step of its task, and " +
    "names in `commit` the head it sends back. That move is its acceptance's changes at that head, which the daemon takes only at the " +
    "pull request's current head (VERDICT_HEAD_NOT_CURRENT). That move counts no review round: only a review that requests changes counts one. ",
  reviewVerdict:
    "review_verdict answers the daemon's request, when the head of the pull request moves once a review step of the issue's task has " +
    "passed, for a verdict on the new head from the agent of each review step of that task that has passed: the reviewer, and the " +
    "owning architect on the issue whose acceptance it ran. Send `verdict` approve or changes, `commit` that head, and `summary`; an " +
    "architect names in `issue` the issue it accepted, and a reviewer leaves `issue` out. It writes no handoff and ends no phase. The " +
    "daemon takes it only from such an agent (VERDICT_CALLER_FORBIDDEN), only once its own step has passed, never for the step that " +
    "runs now (VERDICT_NOT_PAST_REVIEW), whose verdict is the reviewer's completion or the owner's sign_off or move back, and only on " +
    "the pull request's current head (VERDICT_HEAD_NOT_CURRENT). ",
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
  "record or commit";

/**
 * The `legion` tool's description on every host, one template: what the tool is, then `before`,
 * then `handoff_complete`'s contract, then `after`, then where a question for another role goes,
 * `envoyPublish` being the host's name for Envoy's publish tool. `before` and `after` are whole
 * sentences, each ending in a space: the host's sentences on its other operations, and on
 * `read_record`, `read_release_records`, `request_backward_move` and `review_verdict` when it
 * states them (`PHASE_OP_SENTENCES`).
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
    "and the checking role's report on a look of its closed issue's production check) reports this phase complete: `summary` (two sentences for the " +
    "architect), and `verdict` on a phase completion, which is a review step's alone, approve|changes (approve: the head you " +
    "reviewed may merge; changes: it needs changes), on the head your record names, which the call reports as `commit`. The reviewer sends it on " +
    "its completion of reviewing. The architect that owns an issue runs its acceptance step and completes no phase: its sign_off is its approve, " +
    "and its move of the issue back to an earlier phase is its changes. Every other phase completion carries no verdict: Dispatch's check states at the tested head " +
    "decide the testing phase, and the daemon refuses a verdict from the tester (VERDICT_NOT_TAKEN) and from every other role " +
    "(VERDICT_ROLE_FORBIDDEN). The daemon refuses a review step's verdict with no head (VERDICT_COMMIT_REQUIRED), and a review step's word that " +
    "is neither approve nor changes (REVIEWER_VERDICT_REQUIRED). `verdict` is pass|pending|fail on a production check report (pending: " +
    "production does not carry the merge yet, or carries it with a requirement only the deploy can show still unobserved and a look " +
    "still to follow; on the check's last look, only for an issue with no such requirement). " +
    "A tester's completion may name `return_to`, the place of a step of the task its issue runs, 1 for its first: the step the task goes back " +
    "to should the check states at the head it tested fail its step; without it, the task goes back to the first implementation step before " +
    "the step that failed. Only a tester's completion names it (RETURN_TO_NEEDS_FAIL), a place below 1 is refused (INVALID_RETURN_TO), and " +
    "so is a step the task cannot go back to (RETURN_REFUSED). " +
    "A planning, implementing, testing or reviewing phase ends with a handoff record on the " +
    "Dispatch issue: write it with `dispatch handoff-write --issue <issue> --phase <plan|implement|test|review> --body-file <file>` before " +
    "calling. This call reads from Dispatch the newest record of the phase your role works or, once the issue has moved on, of the phase it left, " +
    "and reports it with `commit`, the head that record's body names " +
    "(`head`, the 40-character lower-case commit the work it reports was done against); in implementing, the implementer's must be the commit its " +
    "workspace stands on, `@-`, pushed to `legion/<issue>@origin`, so push the branch first; retro, the merger's completion and a phase you left with no record, or with an implement record naming no head, report the commit the workspace stands on. It " +
    "refuses, posting nothing, a phase you work with no record, a Dispatch that does not answer, a test record naming no head, since the daemon " +
    "reads the check states at the head it names and refuses a tester's completion naming none (TESTED_HEAD_REQUIRED), and, in implementing, an implement record naming no head, an " +
    "implementer's `@-` not yet on `legion/<issue>@origin`, or an implement record whose head is not that pushed `@-`, each naming the remedy. A production check report " +
    "(the task whose first line is `Production check of <KEY>`, handed once the merge closed the issue to the role the check records) needs no " +
    "handoff and no commit: it carries its verdict and summary alone. A pending report also names `wake_at`, an RFC 3339 time: when you want the " +
    "next look. A pass, a fail, and a pending on the check's last look end the check, so none of them carries `wake_at` " +
    "(CHECK_WAKE_UNEXPECTED), and a pending on any other look is refused without one (CHECK_WAKE_REQUIRED). A report on an issue whose check is " +
    "closed or never opened is refused (CHECK_NOT_OPEN), and so is one from a role the check does not record (CHECK_ROLE_MISMATCH). " +
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
    "before you answer. The daemon replaces the pod and its disk when the round ends. The round ends at the moment the message " +
    "names. It can end earlier, once every role has answered, no role is in a turn and each role's last report names no running " +
    "subagent or background job. No answer delays that moment, and the end of a turn is no answer. If a subagent or a background " +
    "job finishes after you answer, commit and push what it made. Then call ready_for_update again with the same round. " +
    "The call ends no phase. The daemon holds your next task until your process comes back on the new pod, in this same session. " +
    (host.after ?? "") +
    `What a later phase needs goes in your handoff; a question for another live role goes to its role topic with ${host.envoyPublish}.`
  );
}
