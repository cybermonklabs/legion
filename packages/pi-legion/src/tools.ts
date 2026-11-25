import { GATED_STEPS, LEGION_ROLES, type LegionRole, zodSchemaApi } from "@legion/contracts";
import {
  LEGION_PHASES,
  LegionGateRegisterRequest,
  type LegionGrant,
  type LegionIssue,
  type LegionPhase,
  LegionPlanRegisterRequest,
  type LegionState,
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
  pluginBuildOf,
  readBuild,
  unknownFieldsRefusal,
} from "@legion/pi-shared/legion-phase-ops";
import type { PiApi, RegisteredTool, SessionContext, ToolResult } from "@legion/pi-shared/pi-types";
import { toolFailure, toolSuccess } from "@legion/pi-shared/tool-result";
import pkg from "../package.json";

/** This plugin's own build, which `read_build` answers with: the manifest of the module this
 * process loaded, never a path on the image. A relaunch installs the plugin pair fresh under
 * the role's state home and loads it with `--extension`, so `/opt/legion/pi-legion` is another
 * build on every pod made from an older image. Its contract is `daemonContract`'s, read as the
 * module loads. */
const BUILD = pluginBuildOf(pkg, import.meta.dir);

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
 * (`@legion/pi-shared/legion-phase-ops`) are an architect's too, and `read_build` is every
 * session's: the controller holds a claim like any other role and reads its own build the same
 * way. An operation two kinds hold is one `op` value, listed once (`toolSchema`). */
const OPERATIONS: Readonly<Record<LegionToolRole, readonly string[]>> = {
  architect: [
    "register_gate",
    "register_plan",
    "stop_task",
    "release_children",
    "request_backward_move",
    "retry_or_escalate",
    "sign_off",
    "close_root",
    "rerun_child",
    "read_record",
    "read_release_records",
    "handoff_complete",
    "ready_for_update",
    "read_build",
  ],
  "phase-worker": [
    "request_backward_move",
    "read_record",
    "read_release_records",
    "handoff_complete",
    "ready_for_update",
    "read_build",
  ],
  controller: [
    "read_build",
    "read_state",
    "set_status",
    "fresh_session",
    "compact",
    "pause",
    "resume",
    "replay_give_up",
    "discard_give_up",
    "set_admission_cap",
    "clear_admission_cap",
  ],
};

/** The fields each operation beyond the phase ops takes; a phase op's are `PhaseOpArguments`'.
 * `read_build` is a phase op and has a line here too: a controller session runs it on this
 * file's own branch rather than through `executePhaseOp`, so this is where its one check that
 * it takes no field stands. */
const OPERATION_FIELDS: Readonly<Record<string, readonly string[]>> = {
  register_gate: ["issue", "artifactId", "version", "step"],
  register_plan: ["issue", "artifactId", "version"],
  stop_task: ["issue", "task"],
  release_children: ["issues"],
  retry_or_escalate: ["issue", "decision", "reason"],
  sign_off: ["issue"],
  close_root: ["issue", "reason"],
  rerun_child: ["issue"],
  read_build: [],
  read_state: ["issue", "issues", "phase", "settings"],
  set_status: ["issue", "status"],
  fresh_session: ["issue", "role"],
  compact: ["issue", "role"],
  pause: ["reason"],
  resume: [],
  replay_give_up: ["giveUp", "consumer", "throughSeq"],
  discard_give_up: ["giveUp", "consumer", "throughSeq", "reason"],
  set_admission_cap: ["cap", "reason"],
  clear_admission_cap: ["reason"],
};

const ISSUE_STATUSES = ["todo", "backlog", "icebox"] as const;

const jsonSuccess = (details: Readonly<Record<string, unknown>>): ToolResult =>
  toolSuccess(JSON.stringify(details), details);

/** `read_state`'s filters, each naming the full records it answers with, in the order the schema
 * declares them. Naming none answers the summary. */
const STATE_FILTERS = ["issue", "issues", "phase", "settings"] as const;

/** One claim as the summary shows it, the architect's or a phase worker's. */
type SummaryClaim = NonNullable<LegionIssue["architect"]>;

/** Who acted on a pause, in the summary's words: the controller with `session`, the session it
 * acted from, or the operator where the entry names none. */
const pauseActorWords = (session: string | undefined): string =>
  session === undefined ? "the operator" : `the controller (session ${session})`;

/** A person's words as the summary quotes them: JSON's quoting, with U+0085, U+2028 and U+2029
 * escaped too, which JSON leaves raw and Go's `%q` in `legion state` escapes. A newline or an
 * instruction in the words reads as the text it is and cannot pose as a line of the summary. */
const quotedWords = (words: string): string =>
  JSON.stringify(words).replace(/[\u0085\u2028\u2029]/g, (separator) =>
    separator === "\u0085" ? "\\u0085" : separator === "\u2028" ? "\\u2028" : "\\u2029"
  );

/** A pause as the summary's line names it: its id, since when, who paused and why, quoted
 * (`pauseWords` in `packages/daemon/cmd/legion/project.go`). */
const pauseWords = (id: number, since: string, by: string, reason: string): string =>
  `pause ${id} since ${since} by ${by}: ${quotedWords(reason)}`;

/**
 * The summary's line on the project's pause, in the words `legion state` prints for an operator
 * (`pausedSummary` and `lastPauseSummary` in `packages/daemon/cmd/legion/project.go`), so the
 * controller and an operator read the same words. The times are not the same text: `legion
 * state` prints each at whole seconds, and this line prints the daemon's own string. While the
 * project is paused: the pause, since when, by whom and why, the roles whose stop it still waits
 * on, each by issue and role, and how many rows it holds, which `legion state` leaves to its
 * JSON. While the project runs after a pause has ended: that pause, then when and by whom it was
 * ended. The state carries one of the two at most, and neither before the project's first pause,
 * when the summary has no such line.
 *
 * Each field is taken off the pause it belongs to, under the state's own names, which
 * `LEGION_ANSWER_READS` declares as `paused.id`, `paused.by|0.kind` and
 * `lastPause.endedBy|0.session` among others.
 */
function pauseSummary(state: LegionState): string | undefined {
  const paused = state.paused;
  if (paused !== undefined) {
    const roles = paused.waitsOn.map((stop) => `${stop.issue} ${stop.role}`);
    const waitsOn =
      roles.length === 0
        ? "no role"
        : `${roles.length} ${roles.length === 1 ? "role" : "roles"} — ${roles.join(", ")}`;
    const rows = paused.held.length;
    const holds = rows === 0 ? "no row" : `${rows} ${rows === 1 ? "row" : "rows"}`;
    const by = pauseActorWords(paused.by.kind === "controller" ? paused.by.session : undefined);
    const pause = pauseWords(paused.id, paused.since, by, paused.reason);
    return `paused: ${pause}; waits on ${waitsOn}; holds ${holds}`;
  }
  const lastPause = state.lastPause;
  if (lastPause !== undefined) {
    const by = pauseActorWords(
      lastPause.by.kind === "controller" ? lastPause.by.session : undefined
    );
    const endedBy = pauseActorWords(
      lastPause.endedBy.kind === "controller" ? lastPause.endedBy.session : undefined
    );
    const pause = pauseWords(lastPause.id, lastPause.since, by, lastPause.reason);
    return `last pause: ${pause}; ended ${lastPause.endedAt} by ${endedBy}`;
  }
  return undefined;
}

/**
 * The summary's line on the wakes the daemon's pacing holds until their turn
 * (`wakesWaiting`): how many, and after `next` the earliest moment any of them is due, which is
 * when the pacing lets the next one out. The state lists them in claim order, so the earliest is
 * searched for; it is compared as a time and printed as the daemon wrote it. `wakes waiting 0` is
 * a state outside every wave. A state that carries no such key has no such line, because it says
 * nothing of what waits, where a 0 would say that nothing does: the daemon leaves the key off the
 * state it serves a caller it reads as below the contract that added the key (`api.stateFor`,
 * `withoutWakesWaiting`; `LegionStateResponse` says which callers those are). The loop runs over
 * the state's own list, so each `dueAt` is read under `wakesWaiting`, the name
 * `LEGION_ANSWER_READS` declares.
 */
function wakesWaitingSummary(state: LegionState): string | undefined {
  if (state.wakesWaiting === undefined) return undefined;
  let next: string | undefined;
  let nextAt = Number.POSITIVE_INFINITY;
  for (const wake of state.wakesWaiting) {
    const dueAt = Date.parse(wake.dueAt);
    if (dueAt < nextAt) {
      nextAt = dueAt;
      next = wake.dueAt;
    }
  }
  return `wakes waiting ${state.wakesWaiting.length}${next === undefined ? "" : ` next ${next}`}`;
}

/**
 * The summary's lines on the intake's open give-ups (`intakeGiveUps`): the messages the daemon's
 * intake gave up on that no replay or discard has closed. One line for each consumer with any
 * open, with how many and the newest stream sequence among them, which is the `throughSeq` of a
 * replay_give_up or discard_give_up of all of them; `intake give-ups (none)` while none is open.
 * A state that carries no such key has no such line, as with `wakesWaiting`: the daemon leaves
 * the key off the state it serves a caller it reads as below the contract that added it
 * (`api.stateFor`, `withoutIntakeGiveUps`). Each field is taken off the state's own
 * `intakeGiveUps` and the `consumers` under it, the names `LEGION_ANSWER_READS` declares.
 */
function intakeGiveUpsSummary(state: LegionState): readonly string[] {
  const giveUps = state.intakeGiveUps;
  if (giveUps === undefined) return [];
  if (giveUps.consumers.length === 0) return ["intake give-ups (none)"];
  return giveUps.consumers.map(
    (consumer) =>
      `intake give-ups ${consumer.name} open ${consumer.open} newest seq ${consumer.newestSeq}`
  );
}

/** The first line of every summary: what the lines below it are, since they are not JSON. */
const SUMMARY_LEGEND =
  "legion state summary. Lines: project; paused: while the project is paused (the pause, since " +
  "when, by whom and why, the roles whose stop it still waits on, how many rows it holds) or " +
  "last pause: once a pause has ended (that pause, then when and by whom it was ended); " +
  "admission cap <cap in force> [free <n>] [ceiling <admission_ceiling>]; cap set <cap> by <who> at " +
  '<time>: "<reason>", quoted, the cap the controller or the operator set while the daemon runs ' +
  "and that stands, absent where none stands; active/waiting/trees/blocked/held, trees <root>... naming the root of every " +
  "live tree and held <issue> <reason> [by <issue>...] one per issue the workflow holds that is " +
  "neither active nor waiting; wakes waiting <count> [next <earliest due time>], the " +
  "wakes the daemon's pacing holds until their turn, 0 outside a wave; " +
  "intake give-ups <consumer> open <count> newest seq <stream sequence>, one line for each " +
  "consumer with messages the daemon's intake gave up on and no replay or discard has closed, " +
  "or intake give-ups (none); " +
  "capability <name> <status> [detail] [config <legion.yaml line>]; issues <count>; then " +
  "<KEY> <phase> <status> for every issue the daemon records, and under each issue Legion still " +
  "runs its slot, hold, gate <current> approved <version|none> and <role> <claim state> " +
  "<last turn start> <last turn end> lines, - for a value the daemon has none for. " +
  "read_state with issue, issues, phase or settings returns the full records it names.";

/**
 * The whole state as the controller reads it between its filtered reads: the project, its pause
 * or its last pause, the admission lists, the wakes the pacing holds, each consumer's open
 * give-ups, each capability's status, one line per recorded issue, and under each issue Legion
 * still runs its slot, its hold, its gate and its claims.
 *
 * Line-oriented, not JSON: the INFRA deployment's state is about 160 KB of JSON for the 92 issues
 * it records, and the tool's output reaches the controller cut at about 41 KB
 * (dispatch://INFRA-329), so the controller acted on a read whose middle it never saw. Every
 * recorded key is named, whatever the size: a summary that drops issues to fit is the bug this
 * replaces. What costs the bytes is the per-claim detail, so only an issue Legion still runs
 * carries it — a `done` issue's claims are retired and an `unrecorded` one (an operator's claim)
 * has none — and `read_state` with a filter answers the rest in full.
 */
function stateSummary(state: LegionState): string {
  const { admission, daemon } = state;
  const lines = [SUMMARY_LEGEND];
  // Every field but the issues is optional on the reader, because the daemon serves a caller
  // below the contract that added one the body without it (dispatch://INFRA-2694). A line
  // whose fact the body does not carry is left out rather than printed empty.
  if (daemon !== undefined) {
    lines.push(`project ${daemon.project} boots ${daemon.boots} started ${daemon.startedAt}`);
  }
  const pause = pauseSummary(state);
  if (pause !== undefined) lines.push(pause);
  if (admission !== undefined) {
    const free = admission.free === undefined ? "" : ` free ${admission.free}`;
    const ceiling = admission.ceiling === undefined ? "" : ` ceiling ${admission.ceiling}`;
    lines.push(`admission cap ${admission.cap}${free}${ceiling}`);
    const override = admission.override;
    if (override !== undefined) {
      const by = pauseActorWords(
        override.setBy.kind === "controller" ? override.setBy.session : undefined
      );
      lines.push(
        `cap set ${override.cap} by ${by} at ${override.setAt}: ${quotedWords(override.reason)}`
      );
    }
    lines.push(
      `active ${admission.active?.join(" ") || "(none)"}`,
      `waiting ${admission.waiting?.join(" ") || "(none)"}`
    );
    // The daemon leaves `trees` out of a body it strips for a caller below the contract that
    // added it, and an absent list says nothing of which trees run, where `trees (none)` would
    // say that none does.
    if (admission.trees !== undefined) {
      lines.push(`trees ${admission.trees.join(" ") || "(none)"}`);
    }
    for (const blocked of admission.blocked ?? []) {
      lines.push(`blocked ${blocked.issue} on ${blocked.blockedBy?.join(" ") ?? "(none)"}`);
    }
    for (const held of admission.held ?? []) {
      const by = held.heldBy === undefined ? "" : ` by ${held.heldBy.join(" ")}`;
      lines.push(`held ${held.issue ?? "-"} ${held.reason ?? "-"}${by}`);
    }
  }
  const wakesWaiting = wakesWaitingSummary(state);
  if (wakesWaiting !== undefined) lines.push(wakesWaiting);
  lines.push(...intakeGiveUpsSummary(state));
  for (const capability of state.capabilities ?? []) {
    // A row that is not `present` is one someone acts on, so it keeps the fact its check found
    // and the legion.yaml line that would record a decision; no filter answers for capabilities.
    const detail = capability.status === "present" ? "" : ` ${capability.detail}`;
    const config = capability.configLine === undefined ? "" : ` config ${capability.configLine}`;
    lines.push(`capability ${capability.name} ${capability.status}${detail}${config}`);
  }
  const keys = Object.keys(state.issues).sort();
  lines.push(`issues ${keys.length}`);
  for (const key of keys) {
    const issue = state.issues[key];
    if (issue === undefined) continue;
    lines.push(`${key} ${issue.phase} ${issue.status}`);
    if (issue.phase === "done" || issue.phase === "unrecorded") continue;
    if (issue.slot !== undefined) {
      const lent = issue.slot.lentTo === undefined ? "" : ` lent ${issue.slot.lentTo}`;
      lines.push(`  slot ${issue.slot.index} since ${issue.slot.admittedAt}${lent}`);
    }
    if (issue.holdReason !== undefined) lines.push(`  hold ${issue.holdReason}`);
    if (issue.designGate !== undefined) {
      const gate = issue.designGate;
      lines.push(`  gate ${gate.currentVersion} approved ${gate.approvedVersion ?? "none"}`);
    }
    const claims: Array<readonly [string, SummaryClaim]> = [];
    if (issue.architect !== undefined) claims.push(["architect", issue.architect]);
    for (const role of LEGION_ROLES) {
      const claim = issue.workers?.[role]?.claim;
      if (claim !== undefined) claims.push([role, claim]);
    }
    for (const [role, claim] of claims) {
      // A working claim whose last turn began hours ago is wedged; an idle claim's end is when
      // it last did anything, and a dash is a turn the daemon recorded none of.
      lines.push(
        `  ${role} ${claim.state} ${claim.lastTurnStart ?? "-"} ${claim.lastTurnEnd ?? "-"}`
      );
    }
  }
  return lines.join("\n");
}

/** `read_state`'s answer: the summary when the call names no filter, and the full records the one
 * filter it names selects. Two filters are refused: each answers a different shape. */
function readState(state: LegionState, parameters: Record<string, unknown>): ToolResult {
  const named = STATE_FILTERS.filter((filter) => parameters[filter] !== undefined);
  if (named.length === 0) return toolSuccess(stateSummary(state));
  if (named.length > 1) {
    throw new Error(
      `read_state takes one of ${STATE_FILTERS.join(", ")}, not ${named.join(" and ")}`
    );
  }
  if (named[0] === "settings") {
    if (parameters.settings !== true) throw new Error("read_state's settings is true");
    return jsonSuccess({ settings: state.settings });
  }
  if (named[0] === "phase") {
    const phase = LEGION_PHASES.find((candidate) => candidate === parameters.phase);
    if (phase === undefined) {
      throw new Error(`read_state's phase is one of ${LEGION_PHASES.join(", ")}`);
    }
    const selected = Object.entries(state.issues).filter(([, issue]) => issue.phase === phase);
    return jsonSuccess({ issues: Object.fromEntries(selected) });
  }
  let keys: readonly string[];
  if (named[0] === "issue") keys = [requiredString(parameters, "read_state", "issue")];
  else {
    const issues = parameters.issues;
    if (!Array.isArray(issues) || !issues.every((issue) => typeof issue === "string")) {
      throw new Error("read_state's issues is a list of issue keys");
    }
    keys = issues;
  }
  const missing = keys.filter((key) => state.issues[key] === undefined);
  if (missing.length > 0) throw new Error(`The daemon has no record for ${missing.join(", ")}`);
  const records = keys.map((key) => [key, state.issues[key]] as const);
  return jsonSuccess({ issues: Object.fromEntries(records) });
}

/** The tool's one object schema, `op` choosing the operation; its fields and the `op` values are
 * in the order `tools.test.ts` pins. */
function toolSchema(pi: PiApi): unknown {
  const z = pi.zod;
  const phaseOp = phaseOpParameters(z);
  return z.object({
    // `read_build` belongs to the architect's roster and the controller's, and the enum names
    // every value once, so the two rosters are joined through a set.
    op: z.enum([...new Set([...OPERATIONS.architect, ...OPERATIONS.controller])]),
    issue: phaseOp.issue,
    since: phaseOp.since,
    artifactId: z
      .string()
      .describe(
        "register_gate's document for the step it registers, and register_plan's plan: its artifact id, slug, or filename, as a dispatch command's --artifact takes it"
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
    task: z
      .string()
      .describe(
        "stop_task's task: the id of its plan_task block in the plan Legion runs for the issue, default for the project's default steps"
      )
      .optional(),
    to: phaseOp.to,
    // The why of the operations that take one, a string as the phase op's own `reason` is.
    // Agents sent it to rerun_child too, whose daemon route keeps no why, so the field says where
    // that why goes.
    reason: z
      .string()
      .describe(
        "close_root's and request_backward_move's why, retry_or_escalate's optional why, the controller's pause's why, discard_give_up's why, which it requires, and the controller's set_admission_cap's and clear_admission_cap's why. No other operation takes it, replay_give_up included. rerun_child takes only issue: say why in a dispatch comment on the child"
      )
      .optional(),
    decision: z.enum(["retry", "escalate"]).optional(),
    summary: phaseOp.summary,
    verdict: phaseOp.verdict,
    wake_at: phaseOp.wake_at,
    plan_rating: phaseOp.plan_rating,
    plan_reason: phaseOp.plan_reason,
    return_to: phaseOp.return_to,
    round: phaseOp.round,
    status: z.enum(ISSUE_STATUSES).optional(),
    phase: z.enum([...LEGION_PHASES]).optional(),
    settings: z.boolean().optional(),
    role: z.enum(LEGION_ROLES).optional(),
    // The give-ups replay_give_up and discard_give_up take: one record by its id, or a consumer's
    // open records through a stream sequence, checked whole before a grant is minted
    // (`giveUpSelection`), as `version` is.
    giveUp: z
      .number()
      .describe(
        "replay_give_up's and discard_give_up's one give-up, a message the daemon's intake gave up on: its record id, as the intake-gave-up notice names it. Name it, or consumer and throughSeq, never both"
      )
      .optional(),
    consumer: z
      .string()
      .describe(
        "replay_give_up's and discard_give_up's intake consumer, as read_state's summary names it on an intake give-ups line: with throughSeq, every open give-up of that consumer at or below that stream sequence"
      )
      .optional(),
    throughSeq: z
      .number()
      .describe(
        "With consumer: the highest stream sequence replay_give_up or discard_give_up takes. read_state's summary gives each consumer's newest seq, which takes all of its open give-ups"
      )
      .optional(),
    // A whole number of at least 1, built through the contracts' schema API as the phase ops'
    // numbers are, since the host's zod types its numbers' bounds as optional.
    cap: zodSchemaApi(z)
      .number({ int: true, min: 1 })
      .describe(
        "set_admission_cap's cap: how many issues the daemon may have started at once, from 1 to the deployment's ceiling, admission.ceiling in read_state"
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

/** A whole number from 1: a give-up's record id, a stream sequence or a gate's version. */
const isWholeFromOne = (value: unknown): value is number =>
  typeof value === "number" && Number.isSafeInteger(value) && value >= 1;

/** The give-ups `operation` (replay_give_up or discard_give_up) names: `giveUp`, one record by its
 * id, or `consumer` with `throughSeq`, every open record of that consumer at or below that stream
 * sequence. Exactly one form, as the daemon takes it (`GIVE_UP_REQUEST_INVALID` otherwise), so a
 * call that names both, or neither, is refused here, before a grant is minted. */
function giveUpSelection(
  parameters: Record<string, unknown>,
  operation: string
): { readonly giveUp: number } | { readonly consumer: string; readonly throughSeq: number } {
  const { giveUp, throughSeq } = parameters;
  const bulk = parameters.consumer !== undefined || throughSeq !== undefined;
  if (giveUp !== undefined && bulk) {
    throw new Error(`${operation} takes giveUp, or consumer and throughSeq, not both`);
  }
  if (giveUp !== undefined) {
    if (!isWholeFromOne(giveUp)) {
      throw new Error(`${operation}'s giveUp is a give-up's record id, a whole number from 1`);
    }
    return { giveUp };
  }
  if (!bulk) throw new Error(`${operation} requires giveUp, or consumer and throughSeq`);
  const consumer = requiredString(parameters, operation, "consumer");
  if (!isWholeFromOne(throughSeq)) {
    throw new Error(`${operation} requires throughSeq with consumer, a stream sequence from 1`);
  }
  return { consumer, throughSeq };
}

/** Refuses every field `operation` does not take, in the phase ops' wording
 * (`unknownFieldsRefusal`). A `reason` refused on rerun_child also says where its why goes:
 * agents send one there, and the daemon keeps no why for it. */
function assertOperationInput(parameters: Record<string, unknown>, operation: string): void {
  const fields = OPERATION_FIELDS[operation];
  if (fields === undefined) throw new Error(`Unsupported legion operation: ${operation}`);
  const refused = Object.keys(parameters).filter((name) => name !== "op" && !fields.includes(name));
  if (refused.length === 0) return;
  const refusal = unknownFieldsRefusal(operation, refused, fields);
  throw new Error(
    operation === "rerun_child" && refused.includes("reason")
      ? `${refusal}. Say why in a dispatch comment on the child`
      : refusal
  );
}

/** The daemon's role-local workflow surface: no operation can schedule a worker. Every operation
 * that writes mints its own grant in-process and posts it with the request; nothing is written
 * to the pane. The phase ops run on the executor every harness shares (`executePhaseOp`):
 * `handoff_complete` is refused here only for the root architect's session, and finds what it
 * reports in the pane first (`handoffReport`); the daemon refuses an architect's completion
 * (ACCEPTANCE_IS_SIGN_OFF where nothing else refuses it first), so it is a phase worker's
 * operation. The operations of `OPERATIONS.controller` belong to the controller. */
export function createLegionTool(deps: {
  readonly pi: PiApi;
  readonly daemon: () => LegionDaemonClient;
  readonly session: (context: SessionContext) => LegionToolSession;
  /** Mints the controller's grant, authenticated by its registration's secret. */
  readonly controllerGrant: (sessionId: string) => Promise<LegionGrant>;
  /** Told of each `handoff_complete` that succeeded: the session's phase is complete. */
  readonly onPhaseCompleted: (context: SessionContext) => void;
  /** Told of each `ready_for_update` that succeeded: the session has answered the daemon's
   * update message on its pod, and its phase stays open. */
  readonly onUpdateAnswered: (context: SessionContext) => void;
  /** What a completion by `role` reports beside its summary while the issue stands at `phase`
   * (`findHandoffReport`): for a file-backed phase the role works or, once the issue has moved
   * on, the one it left, the newest Dispatch handoff record of that phase and the head its body
   * names under `head`, which for the implementer's round, while the issue stands in
   * implementing, must be the commit its workspace stands on, pushed to `legion/<issue>@origin`;
   * otherwise, and for a left phase with no record or an implementer's left record naming no head,
   * the commit the workspace stands on, alone. Throws
   * naming the remedy when the phase the role works has no record, Dispatch does not answer, or,
   * in implementing, the implementer's record names no head, the commit its workspace stands on
   * is not pushed, or its record's head is not that pushed commit. */
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
    // The shared sentence on read_record, since its name does not say that it reads the caller's
    // own issue when the call names none; the shared sentence on read_release_records before
    // handoff_complete's contract, since the records it answers are not read from its name;
    // after that contract the shared sentences on request_backward_move (an architect's names
    // the issue it returns from accepting) and read_build, then the architect's
    // retry_or_escalate, its register_plan and stop_task, its sign_off and the controller's
    // operations: the text tools.test.ts pins, stop_task's and sign_off's by their terms and the
    // rest exactly, so a change to the shared template cannot reach an Oh My Pi agent unnoticed.
    // The rules for ending a child are the words of the daemon's record.EndChildRule, and the
    // sentence after them is the daemon's record.StatusNotTakenYet (both in
    // packages/daemon/internal/record/project.go). These tests hold the copies together: the
    // daemon's TestTheEndChildRuleGolden writes both texts to
    // packages/contracts/fixtures/daemon-api/end-child-rule.json, and tools.test.ts holds this
    // description to that file.
    description: legionToolDescription({
      envoyPublish: "envoy_publish",
      before: PHASE_OP_SENTENCES.readRecord + PHASE_OP_SENTENCES.readReleaseRecords,
      after:
        PHASE_OP_SENTENCES.requestBackwardMove +
        PHASE_OP_SENTENCES.readBuild +
        "retry_or_escalate decides an issue the daemon holds after its worker failed (issue, decision): retry puts the issue " +
        "back in the phase it was held from and starts that phase's worker again, escalate leaves it held for the controller, " +
        "and an optional reason says why you decided as you did: a retry writes it into the restarted worker's task and the " +
        "daemon's own log line for the release, and an escalation sends it to the controller with the hold. " +
        "register_plan (issue, artifactId, version) gives Legion the plan of an issue you own: a document on that issue, by its id, slug " +
        "or filename as a dispatch command's --artifact takes it, at a version Dispatch has. Its one `plan_task` block names in `steps` the " +
        "steps its task runs, in the order they run: some of `planner implementer tester reviewer acceptance:owner`, with `implementer` " +
        "in it and before each `tester`, `reviewer` or `acceptance:owner` step; a " +
        "task that names none runs the project's default steps, and Legion runs the retro and the merge itself. Give it before the issue's " +
        "work starts: a root's before its design gate opens, a child's before release_children releases it. The answer is the plan Legion " +
        "took, each task with its steps. Legion records nothing when it refuses a plan it does not carry out, with one reason for each cause " +
        "(PLAN_REFUSED, which also refuses a version that changes a running task: stop it with stop_task first, and a plan of an issue " +
        "that started its work before Legion took plans), a version Dispatch does " +
        "not have (VERSION_NOT_IN_DISPATCH), a version no later than the plan in force (PLAN_VERSION_OLD), another document than the plan " +
        "in force (PLAN_DOCUMENT_CHANGED), a document of another issue (ARTIFACT_NOT_ON_ISSUE), an issue outside your tree " +
        "(ISSUE_OUTSIDE_TREE), an issue the daemon has not recorded whose Dispatch status is not triage, backlog, icebox or todo " +
        "(ISSUE_ALREADY_RELEASED), a version that changes a task that is done (TASK_DONE), and a plan given for a run of the issue " +
        "that ended before Legion applied it: give it again (PLAN_STALE_GENERATION). " +
        "Legion takes no plan for a recorded issue whose experiment's group runs the project's default steps (PLAN_NOT_TAKEN, naming " +
        "the group): it records nothing, and the same plan given again gets the same answer. " +
        "stop_task (issue, task) stops one task of the plan Legion runs for a recorded issue: its running step stops, and the issue is " +
        "held, `holdReason` task-stopped, until a later version of the plan, given with register_plan, changes or replaces that task, " +
        "or, on a root, a person sets the root todo; the task then starts at its first step (a task that never started waits for its " +
        "gate and its line, as any task does before its first step); retry_or_escalate does not start it again (TASK_STOPPED). On an " +
        "issue whose first step has not run, the stop first settles the plan that step would walk, the newest plan you gave or the " +
        "project's default steps (task default), and then stops its task. The daemon refuses a task the plan does not hold " +
        "(TASK_NOT_FOUND, naming the plan's tasks), one that is done (TASK_DONE), a stop in a tree that lingers after it left the " +
        "workflow (TREE_LINGERING), every task of an issue whose experiment's group runs the project's default steps, since Legion " +
        "takes no plan from its owner (PLAN_NOT_TAKEN), and a stop of a root that waits on a person before its first step " +
        "(TASK_AWAITING_PERSON: change its task with a later version of the plan, or stop it once the person has answered). It also " +
        "refuses, changing nothing, a stop of an issue outside your tree or one that left it (ISSUE_OUTSIDE_TREE), and a stop of a run " +
        "of the issue that ended before Legion applied it: read the issue again (TASK_STALE_GENERATION). " +
        "sign_off is the architect's acceptance of an issue in accepting, moving its task to its next step, or to retro where the " +
        "acceptance is the task's last step. The daemon refuses, changing nothing, a " +
        "sign-off of an issue you no longer own (ISSUE_NOT_OWNED, or ISSUE_OUTSIDE_TREE once it left your tree), one of a run of the " +
        "issue or of accepting that ended before Legion applied it (SIGNOFF_STALE_GENERATION, SIGNOFF_STALE_ENTRY: read the issue and " +
        "its evidence again) and one in every other phase (SIGNOFF_OUTSIDE_ACCEPTING); it also refuses it while an issue beneath it " +
        "is still in the workflow (SIGNOFF_CHILD_IN_WORKFLOW, naming each one), since a parent is accepted only once no issue beneath " +
        "it is still in the workflow. " +
        "Sign off again when each of them has left the workflow. " +
        "The daemon takes the owning architect's done, backlog, icebox or triage on a child as it takes a person's move. " +
        "End a child with your own status write in Dispatch. " +
        "Write done with a reason when the child's work is done, folded into another issue or no longer needed: " +
        "dispatch issue-update --issue <KEY> --status done --reason '<why>'. " +
        "For a fold, name the carrier in the reason: the issue that carries the work. " +
        "Before the done of a fold, make each issue that is blocked_by the child wait on the carrier instead. " +
        "Done starts those issues, and the Referenced by list of dispatch read names each of them as blocked_by. " +
        "Before done on a running child, have its worker push the work that the carrier needs. " +
        "Done ends the worker's turn and the pod at once. " +
        "Unless the work of the child's open pull request should land, close that pull request with your own App (gh pr close). " +
        "When it is in the merge queue, first take it out (GraphQL dequeuePullRequest). " +
        "Do this before done, or at once after a done that you wrote first. " +
        "Write backlog or icebox only when a person asked for that move, or as the first step of a restart. " +
        "A restart is backlog, then rerun_child. " +
        "Say why in a dispatch comment. " +
        "Write no status on a child that is already done, because a move out of done ends an open production check. " +
        "After each move, read the child back (dispatch read --issue <KEY>). " +
        "When the daemon set the move back, write it again. " +
        "A status that you wrote and that the daemon has not taken yet does not count until read_record shows it. " +
        "When a child can neither finish nor take one of these moves, ask the person. Hold the acceptance until they answer. " +
        "close_root is refused the same way and with the same code while an issue beneath the root is still in the workflow, " +
        "because closing it would stop that work mid-turn. Answer that refusal the same way. " +
        "release_children and rerun_child are refused for a child whose ancestor is in retro or merging " +
        "(CHILD_UNDER_ACCEPTED_PARENT, naming the ancestor), since the head that ancestor's last step passed cannot carry work released " +
        "under it now: wait for the ancestor to close, then release it, where that ancestor is not the tree's root. " +
        "The controller's read_state with no field returns a summary of the daemon's whole state — the project, the pause " +
        "that stands or the last one ended, admission, how many wakes the daemon's pacing holds and when the next is due, how " +
        "many give-ups each intake consumer has open and the newest stream sequence among them, each " +
        "capability, one line per recorded issue, and under each issue Legion still runs its slot, hold, gate and each claim's " +
        "state with its last turn times — and with issue, issues, phase or settings returns the full records it names; " +
        "set_status moves an issue to todo, backlog or icebox. " +
        "With set_status, the controller moves an issue to backlog or icebox only when a person asked for that move, or when the work is done, folded into another issue or no longer needed, and cites that reason in a comment on the issue. " +
        "Two writes are not that move: a root's restart, which sets backlog and then todo in the same turn, and the " +
        "controller's report issue's icebox, because that issue holds no work. " +
        "fresh_session (issue, role) has one claim leave the session it runs in behind and start again in a new one: the daemon " +
        "stops the agent if it runs, forgets the session, and starts the claim, which loses everything that session held. A " +
        "phase worker that comes back owing no task is started through its issue's current phase, so it has that phase's work. " +
        "The answer is empty when the claim came back and nothing else is to be said. phaseNotRestarted says why nothing " +
        "restarted the claim's phase, where the claim came back owing no task and the workflow refused that restart: the " +
        "claim is running, and no work was handed to it. It says what was refused, where this request stopped the claim " +
        "and a step that would have started it again was refused, and the state that left the claim in: read that state " +
        "in read_state, and ask for a fresh session again only where the claim is suspended. " +
        "It reaches a claim stored working that no process stands behind: the daemon reads that claim's process when the " +
        "request arrives, suspends the claim where it stands, and starts it again with the task its dead turn held. " +
        "The daemon refuses it for a claim whose process that read finds running " +
        "(CLAIM_IN_A_TURN), for a claim whose session is being compacted, which is not read at " +
        "all (CLAIM_IN_A_TURN), for a claim whose process the runtime would not answer for, for " +
        "one that records no process to read, and for one that moved under the request and that a second read " +
        "finds no longer working or gone once more, each left exactly as the request found it " +
        "(CLAIM_UNSETTLED; a claim that moved is otherwise answered as that second read finds it), for a claim a " +
        "suspension is held over, in whatever state the hold found it (CLAIM_IN_A_TURN where " +
        "the claim is working, CLAIM_UNSETTLED where it is not), for a claim between states — " +
        "one queued, one whose launch is in flight, and one left working under a relaunch the " +
        "wake pacing parked (CLAIM_UNSETTLED) — and for role controller. " +
        "compact (issue, role) shortens one claim's session instead: the daemon has Oh My Pi write a text summary of the " +
        "session so far, and the claim keeps its session, its workspace and everything it knows of its issue and is sent " +
        "whatever task it owes again. Try compact first for a claim whose session a provider keeps refusing, and " +
        "fresh_session for a claim no process stands behind, which compact refuses and fresh_session reaches. The answer " +
        "says whether the session was compacted and, when it was not, why; the claim is back either way. The daemon " +
        "refuses it for a claim that is suspended, failed or retired, and for one stored working " +
        "that no process stands behind (CLAIM_NOT_RUNNING: fresh_session reaches both), for a " +
        "claim whose process the daemon reads as running (CLAIM_IN_A_TURN), for a claim whose " +
        "session is being compacted already (CLAIM_IN_A_TURN), for a claim whose process the " +
        "runtime would not answer for, for one that records no process to read, and for one " +
        "that moved out of working while the request was made " +
        "(CLAIM_UNSETTLED), for a claim a " +
        "suspension is held over (CLAIM_IN_A_TURN where the claim is working, CLAIM_UNSETTLED " +
        "where it is not), for a claim between states — one queued, one whose launch is in " +
        "flight, one whose agent has registered and is not ready yet, which fresh_session " +
        "reaches, and one left working under a relaunch the wake pacing parked " +
        "(CLAIM_UNSETTLED) — for a claim whose role runs on Claude Code (CLAIM_NOT_ON_OMP) " +
        "and for role controller. " +
        "Each refusal says what the claim does now and which operation reaches each state. It " +
        "names no moment to come back at: ask again later, and read the claim's state in " +
        "read_state. " +
        "pause (reason) pauses the whole project, and resume ends its pause; neither takes an issue. While the project " +
        "is paused the daemon admits no issue, and it starts no role and gives no role a task except the controller. Every " +
        "other role stops with its conversation kept, an idle one at once and one in a turn when that turn ends, and the " +
        "pause deletes no pod. Each task, notice and wake for a stopped role waits for the resume. The answer is the " +
        "pause: its id, since, by and reason. The daemon refuses a second pause (PROJECT_ALREADY_PAUSED, naming the " +
        "pause that stands) and a pause its configuration cannot hold (PAUSE_UNAVAILABLE, naming the setting). " +
        "resume ends the pause whoever made it, the operator's included. Each role the pause held something for then " +
        "starts once, at the daemon's wake pacing, and every other role it stopped stays stopped until its next wake. " +
        "The answer is the pause with endedAt and endedBy, and released, how many held rows the resume let out. The " +
        "daemon refuses a resume of a project that is not paused (PROJECT_NOT_PAUSED, naming its last pause when it has one). While " +
        "the project is paused it refuses fresh_session and compact (PROJECT_PAUSED, naming the pause that stands), " +
        "since each would start a turn. " +
        "replay_give_up applies again give-ups, the messages the daemon's intake gave up on because their handling kept " +
        "failing or they did not decode, which the daemon keeps whole. The daemon runs each one through the handling a " +
        "live delivery takes and publishes nothing to the stream, so replay once the fix for a record's last error runs in " +
        "the daemon. Name giveUp, one record's id, as the intake-gave-up notice names it, or consumer and throughSeq, every " +
        "open record of that consumer at or below that stream sequence: read_state's summary gives each consumer's open " +
        "count and newest seq. By its id a replay takes a discarded record too, so a wrong discard can be undone. One call " +
        "takes at most 100 records, in stream order, and goes on past a record that fails. The answer is the daemon's: " +
        "replayed, each record's id, consumer, streamSeq and outcome, and remaining, how many open records of the selection " +
        "are left for another call. applied, already applied and no fact close the record; failed, with its error, leaves " +
        "the record as it was. " +
        "discard_give_up (giveUp, or consumer and throughSeq, and reason) closes the records it names with your reason and " +
        "applies nothing: use it for a message that can never apply. The answer is discarded, the id of each record it " +
        "closed. The daemon refuses a call that names both forms or neither, and a discard whose reason is blank or longer " +
        "than 2,048 characters (GIVE_UP_REQUEST_INVALID), an id of no give-up of this project (GIVE_UP_NOT_FOUND), a replay " +
        "of a record a replay closed and a discard of a closed record, naming how and when it closed (GIVE_UP_CLOSED), an " +
        "id a running replay holds (GIVE_UP_BEING_REPLAYED), and every call to a daemon that runs no intake (NO_INTAKE). " +
        "set_admission_cap (cap, reason) sets how many issues the daemon may have started at once, from 1 to the " +
        "deployment's ceiling (admission.ceiling, legion.yaml's admission_ceiling), and clear_admission_cap (reason) " +
        "returns the project to its starting cap, legion.yaml's admission_cap; neither takes an issue. Each is in force " +
        "from the change's own admission pass on, with no restart: a raise starts the issues waiting for the slots it " +
        "adds, and a lower cap stops nothing that runs, only starting nothing new until fewer issues hold a slot than it " +
        "allows. Change the cap only where a person's words, which you cite in reason, ask for it. The daemon records " +
        "each change with who made it, when, why, and the cap before and after it (admission.capChanges in read_state), " +
        "and a set of the cap that stands or a clear where none stands records nothing. The answer is the cap in force, " +
        "the ceiling and the cap set that stands. The daemon refuses a cap below 1 (INVALID_ADMISSION_CAP), a cap above " +
        "the ceiling (ADMISSION_CAP_ABOVE_CEILING, naming the ceiling: only the operator raises it, in legion.yaml), a " +
        "change with no reason (ADMISSION_CAP_REASON_REQUIRED), a reason longer than 2,048 characters " +
        "(ADMISSION_CAP_REASON_TOO_LONG), a grant that is not a controller's (CONTROLLER_REQUIRED), and a daemon with " +
        "no workflow (ADMISSION_CAP_UNAVAILABLE). ",
    }),
    defaultInactive: true,
    parameters: toolSchema(pi),
    execute: async (_id, parameters, signal, onUpdate, context) => {
      // The writes the daemon applies at most once however often they arrive (handoff_complete,
      // sign_off, register_gate, register_plan, stop_task) wait out its restart until the
      // agent's abort.
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
            { daemon: daemon(), claim: active, build: BUILD, handoffReport, branchHead, restart },
            parameters
          );
          // A call that returned without throwing: a completion, and a backward move, which hands
          // the issue to another phase. Either way this session's phase is no longer open.
          // `ready_for_update` ends no phase: it answers the pod's update message, and the worker
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
          // A controller session holds no claim, so it cannot reach `executePhaseOp`, which
          // mints from one. It answers the same values here: this build's package, version and
          // contract, and the contract the daemon states on the health route.
          if (operation === "read_build") {
            return jsonSuccess(await readBuild({ daemon: client, build: BUILD }));
          }
          if (operation === "read_state") return readState(await client.state(), parameters);
          // The project's pause and its resume name no issue: the daemon acts on its own project.
          // Each is validated whole before its grant is minted, and each answer is the daemon's
          // own: the pause it began, or the pause it ended with how many held rows it let out.
          if (operation === "pause") {
            const reason = requiredString(parameters, operation, "reason");
            const { grantId } = await controllerGrant(active.sessionID);
            return jsonSuccess(await client.projectPause({ grantId, reason }));
          }
          if (operation === "resume") {
            const { grantId } = await controllerGrant(active.sessionID);
            return jsonSuccess(await client.projectResume({ grantId }));
          }
          // The replay and the discard of the intake's give-ups name no issue either. Each is
          // validated whole, its selection and the discard's reason, before its grant is minted,
          // and each answer is the daemon's own: each record the replay took with how it ended and
          // how many are left, or the ids the discard closed.
          if (operation === "replay_give_up") {
            const selection = giveUpSelection(parameters, operation);
            const { grantId } = await controllerGrant(active.sessionID);
            return jsonSuccess(await client.intakeReplay({ grantId, ...selection }));
          }
          if (operation === "discard_give_up") {
            const selection = giveUpSelection(parameters, operation);
            const reason = requiredString(parameters, operation, "reason");
            const { grantId } = await controllerGrant(active.sessionID);
            return jsonSuccess(await client.intakeDiscard({ grantId, ...selection, reason }));
          }
          // The cap's set and its clear name no issue either, and each is validated whole before
          // its grant is minted; each answer is the daemon's own: the cap in force after the change.
          if (operation === "set_admission_cap") {
            const reason = requiredString(parameters, operation, "reason");
            const cap = parameters.cap;
            if (!isWholeFromOne(cap)) {
              throw new Error("set_admission_cap requires cap, a whole number of at least 1");
            }
            const { grantId } = await controllerGrant(active.sessionID);
            return jsonSuccess(await client.admissionCap({ grantId, cap, reason }));
          }
          if (operation === "clear_admission_cap") {
            const reason = requiredString(parameters, operation, "reason");
            const { grantId } = await controllerGrant(active.sessionID);
            return jsonSuccess(await client.admissionCapClear({ grantId, reason }));
          }
          // The controller's three writes of one issue: each validated whole before its grant is
          // minted.
          const issue = requiredString(parameters, operation, "issue");
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
            if (!isWholeFromOne(version)) {
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
          case "register_plan": {
            // The owner gives the plan of any issue of its tree, a child the daemon has not
            // recorded yet included; which issues those are is the daemon's to hold
            // (ISSUE_OUTSIDE_TREE), so the call is checked whole here and nothing more.
            const issue = requiredString(parameters, operation, "issue");
            const reference = requiredString(parameters, operation, "artifactId");
            const version = parameters.version;
            if (typeof version !== "number" || !Number.isSafeInteger(version) || version < 1) {
              throw new Error("register_plan requires a positive integer version");
            }
            // The document is named as register_gate's is: its id, or a reference looked up.
            const isId = LegionPlanRegisterRequest.shape.artifactId.safeParse(reference).success;
            const artifactId = isId ? reference : await resolveDocument(issue, reference);
            // The fact is keyed by the issue, its generation, the document and the version, so a
            // repeat is the registration again, answered the same.
            const taken = await retryWhileDaemonRestarts(
              "register_plan",
              async () =>
                client.planRegister({
                  grantId: await mintClaimGrant(client, active),
                  issue,
                  artifactId,
                  version,
                }),
              restart
            );
            return jsonSuccess(taken);
          }
          case "stop_task": {
            const issue = requiredString(parameters, operation, "issue");
            const task = requiredString(parameters, operation, "task");
            // A repeat after a stop that landed finds the task stopped and changes nothing.
            await retryWhileDaemonRestarts(
              "stop_task",
              async () =>
                client.taskStop({ grantId: await mintClaimGrant(client, active), issue, task }),
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
            // The reason is optional: one given is written into the task the restarted role is
            // given and into the daemon's own log line for the release, and a call that names
            // none is unchanged.
            const reason =
              parameters.reason === undefined
                ? {}
                : { reason: requiredString(parameters, operation, "reason") };
            const grantId = await mintClaimGrant(client, active);
            await client.phaseRetry({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
              decision,
              ...reason,
            });
            return jsonSuccess({});
          }
          case "sign_off": {
            // A repeat after a sign-off that landed finds the issue out of accepting, at its task's next
            // step or in retro, and changes nothing.
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
          case "rerun_child": {
            const grantId = await mintClaimGrant(client, active);
            await client.childRerun({
              grantId,
              issue: requiredString(parameters, operation, "issue"),
            });
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
