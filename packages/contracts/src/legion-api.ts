import { z } from "zod";
import { DISPATCH_KEY_PATTERN, GATED_STEPS, LEGION_ROLES } from "./legion-roles";

/**
 * The Legion daemon's HTTP API, as its readers see it.
 *
 * Go owns this wire shape: `packages/daemon/internal/api` (`state.go`, `operator.go`) and the
 * claim wire in `packages/daemon/internal/claim/wire.go` are the source of truth, and every
 * schema here mirrors them field for field. The two are pinned to each other by
 * `packages/contracts/fixtures/daemon-api/*.json`, written by the Go golden tests
 * (`go test ./internal/api/ -update`, and `go test ./internal/projection/ -update` for
 * `state-controller-claim.json`) and parsed here by `legion-api.test.ts` — no generator runs in
 * either direction, so a Go field added without its line below fails that test.
 *
 * Reached as `@legion/contracts/legion-api`, never from the barrel.
 */

/** The controller's two claim recoveries keep their own module, and every importer reads them
 * from here. */
export {
  LegionClaimRequest,
  LegionCompactResponse,
  LegionFreshSessionResponse,
} from "./legion-claim-recovery";

const nonEmptyString = z.string().min(1);
const issueKey = z.string().regex(DISPATCH_KEY_PATTERN);
/** Go emits RFC 3339 through `time.Time`; a daemon on a non-UTC clock emits an offset. */
const timestamp = z.iso.datetime({ offset: true });

/** The RFC 3339 instant a session sends: seconds required, the fraction optional, `Z` or an
 * offset required, as Go's `time.Time` decode requires them. It is the stricter of the two: it
 * refuses a few strings that decode takes (`+24:00`, a comma before the fraction). `timestamp`
 * above takes `2026-11-06T10:00Z`, which that decode rejects, so a plugin that validated a request
 * field against it would send a body the daemon answers with its generic invalid-body error rather
 * than the field's own guidance. The plugin reads this same schema for both fields a session
 * sends — `wake_at` and `read_release_records`' `since`
 * (`packages/pi-shared/src/legion-phase-ops.ts`). Only a field a session sends reads it: every
 * other field here is the daemon's own output, which Go always writes with seconds. */
export const LegionInstant = timestamp.regex(
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/,
  { error: "an RFC 3339 time with seconds, as 2026-11-02T09:00:00Z is" }
);

/** `api.Phase` — the state an admitted issue sits in, in the transition table's own order. Not
 * the role working it: `workers` is keyed by role (`LEGION_ROLES`). `awaiting_person` is a root
 * that keeps its admission slot while its architect waits on a person's answer (`waitingOn` on
 * the issue record). `accepting` is the architect's acceptance between the reviewer's approval and
 * retro: no worker runs it. The merger merges in `merging`, and the daemon closes the issue when
 * it reads that merge (`done`); the production check after the close is no phase (`check` on the
 * issue record). */
export const LEGION_WORKFLOW_PHASES = [
  "admitted",
  "awaiting_person",
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "retro",
  "merging",
  "integrating",
  "done",
  "held",
] as const;

/** The `api.PhaseBackwardRequest` targets contract-12's daemon accepts. Future state-only phases
 * join this list only with their daemon transition. */
export const LEGION_PHASE_BACKWARD_TARGETS = [
  "admitted",
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "retro",
  "merging",
  "done",
  "held",
] as const;

/** `api.Phase` as the state route reads it: the workflow's phases plus `unrecorded`, which is not
 * one of them — it is what an issue the workflow does not record reads as, where an operator's
 * claim exists and an issue does not. Its status reads the same. No request names it. */
export const LEGION_PHASES = [...LEGION_WORKFLOW_PHASES, "unrecorded"] as const;

export type LegionPhase = (typeof LEGION_PHASES)[number];

/** `supervise.ClaimState` — where a claim is in its life, in the order a launch reaches them. */
export const LEGION_CLAIM_STATES = [
  "queued",
  "launch_uncertain",
  "launching",
  "shim_connected",
  "registered",
  "ready",
  "working",
  "idle",
  "suspended",
  "failed",
  "retired",
] as const;

export type LegionClaimState = (typeof LEGION_CLAIM_STATES)[number];

/**
 * `runtime.Locator` — where a claim's process is: the runtime word, the claim token, the process
 * incarnation, and exactly one backend member, the one the runtime word names. Nested, marshalled
 * by the standard library: `tmux` (the pane on the daemon's private server; the incarnation is
 * `<pane pid>:<start ticks>`) or `sandbox` (one role process in its issue's shared Agent Sandbox
 * pod: the Sandbox's name, the pod's uid, the container (`roles` for an issue pod) and the process generation; the
 * incarnation is `<pod uid>/<generation>`).
 */
const legionLocator = z.discriminatedUnion("runtime", [
  z.object({
    runtime: z.literal("tmux"),
    claim: nonEmptyString,
    incarnation: nonEmptyString,
    tmux: z.object({ window: nonEmptyString, pane: nonEmptyString }),
  }),
  z.object({
    runtime: z.literal("sandbox"),
    claim: nonEmptyString,
    incarnation: nonEmptyString,
    sandbox: z.object({
      namespace: nonEmptyString,
      name: nonEmptyString,
      podUid: nonEmptyString,
      container: nonEmptyString,
      generation: z.number().int().positive(),
    }),
  }),
]);

/** `api.CapabilityReportView` — one session's capability report as the state shows it: when the
 * session measured, the live rows its checks proved (`ok`) and the rows they did not, each with
 * the fact the check found (`open`). `incarnation` is the reporting process's: the daemon keeps a
 * claim's latest report whatever its incarnation, so a reader compares it with
 * `locator.incarnation` to tell a report of the running process from one a relaunch has outlived
 * (contract 21). It is empty for a report the API recorded from a claim whose machine held no
 * process (`runtime.Locator{Claim}`), which no reader compares with a locator. `ok` and `open`
 * are never null; the daemon floors an open row's detail (`capabilities.Normalize`), and the
 * schema stays the guard. */
const legionCapabilityReportView = z.object({
  measuredAt: timestamp,
  incarnation: z.string(),
  ok: z.array(nonEmptyString),
  open: z.array(z.object({ name: nonEmptyString, detail: nonEmptyString })),
});

/** `api.StepView` — what the daemon last saw a claim's agent do, built from the tool frames of
 * its worker stream (contract 57): `at`, when the daemon received that step; `turnStartedAt`,
 * when the turn it belongs to began; `tool` and `callId`, the call the agent is in, which the
 * start and end frames pair by; `argsBytes` and `argsHash`, the length of that call's arguments
 * and an eight-byte fingerprint of them; and `startedAt` and `endedAt`, when the daemon received
 * that call's start and end.
 *
 * The arguments themselves are never on the wire: a shell command routinely holds a credential,
 * and this record reaches every surface an operator reads. Every field is absent where the
 * record holds nothing for it — a claim whose turn has begun and whose agent has called nothing
 * carries `turnStartedAt` alone, and a call still open carries `startedAt` and no `endedAt`,
 * which is the shape a stalled turn leaves behind. */
const legionStepView = z.object({
  at: timestamp.optional(),
  turnStartedAt: timestamp.optional(),
  tool: nonEmptyString.optional(),
  callId: nonEmptyString.optional(),
  argsBytes: z.number().int().positive().optional(),
  argsHash: nonEmptyString.optional(),
  startedAt: timestamp.optional(),
  endedAt: timestamp.optional(),
});

/** `supervise.FailureCauses` — the closed set of words a claim can fail under, in the order the
 * daemon declares them. The Go side is the source: its `TestFailureCausesGolden` writes that
 * list to `fixtures/daemon-api/failure-causes.json`, and `legion-api.test.ts` holds this list
 * equal to the file, so a word added on either side alone fails a test. It matters because the
 * enum below is strict: a cause this list does not name refuses the whole state body at every
 * plugin that reads it. */
export const LEGION_FAILURE_CAUSES = [
  "launch_refused",
  "died",
  "never_registered",
  "stale_plugin",
  "prompt_retires_exhausted",
  "provider_error",
  "unrecorded",
] as const;

/** The one word the site that failed a claim chose (`supervise.FailureCause`). A claim view
 * carries it twice over: in `failure.cause`, why the claim failed, and in `pendingCause`, what
 * failed the process whose relaunch is in flight. */
const legionFailureCause = z.enum(LEGION_FAILURE_CAUSES);

/** `api.ClaimFailureView` — why a claim failed: `cause`, the one word the site that failed it
 * chose (`supervise.FailureCause`), `detail`, the sentence that site wrote beside it, `at`, the
 * moment it failed, and `attempts`, the four counts it failed with, which the limits in
 * `settings` are read against (`launch_failure_limit` and the rest).
 *
 * `at` is absent for a claim whose record carries no moment, which is every failure the
 * migration that added the columns backfilled; `detail` is a plain string for the same reason,
 * since a backfilled failure the store held no sentence for carries an empty one and the body
 * must still read. A claim carries the record from its failure until it is given another run,
 * because that run is what clears it, with the counts the record names: a claim an operator
 * retired after it failed carries it still, which is what lets an agent reading a retired claim
 * say why it is there. */
const legionClaimFailureView = z.object({
  cause: legionFailureCause,
  detail: z.string(),
  at: timestamp.optional(),
  attempts: z.object({
    launchFailures: z.number().int().nonnegative(),
    deaths: z.number().int().nonnegative(),
    promptFailures: z.number().int().nonnegative(),
    promptRetires: z.number().int().nonnegative(),
  }),
});

/** `api.PendingTaskView` — the task a claim holds, as an agent surface shows it: the key's own
 * presence says one is held, `phase` is the issue phase it was queued for (absent for a task of
 * no phase, an operator's own or an architect's), `queuedAt` when it was queued, and
 * `sendFailures` with `lastSendError` the sends that never reached a turn, absent for a task no
 * send has failed for.
 *
 * The task's own text is never here, and this is the difference from `legionDeliveryView`, which
 * the operator routes answer with: what a reader of the state decides on is that a worker holds
 * a task and since when, not the prose another seat was sent. */
const legionPendingTaskView = z.object({
  phase: z.enum(LEGION_WORKFLOW_PHASES).optional(),
  queuedAt: timestamp,
  sendFailures: z.number().int().positive().optional(),
  lastSendError: nonEmptyString.optional(),
});

/** `api.ClaimView` — `session` is empty until the claim's agent registers one, `locator` is
 * absent while no process runs (queued, suspended, failed, retired), `capabilities` is absent
 * until a session of the claim reports on `claims/ready` (the controller's never does), and
 * `failure` is absent for a claim the daemon holds no cause and sentence for. */
const legionClaimView = z.object({
  session: z.string(),
  state: z.enum(LEGION_CLAIM_STATES),
  locator: legionLocator.optional(),
  capabilities: legionCapabilityReportView.optional(),
  /** The run of the last confirmed delivery this claim retired — the run it goes on working
   * until a delivery of another run replaces it — and absent for a claim that has retired no
   * confirmed task of a run. Read against the issue's own `generation` it says whether the work
   * the claim serves is of this generation. */
  servingGeneration: z.number().int().positive().optional(),
  /** 0 = no contract recorded. */
  pluginContract: z.number().int().nonnegative(),
  /** The update round the claim is in, absent while none is open: `pod:<pod uid>`, the drift
   * round every claim of that pod shares, or `plugin:<daemon contract>`, this claim's own. Once
   * the worker has answered `ready_for_update`, the daemon holds the claim's next task until the
   * round ends and the process is replaced (contract 46). */
  updateRound: nonEmptyString.optional(),
  /** The commit the worker reported as its issue branch's head at origin when it answered the
   * round; absent until it answers, and for a worker that reported none. It is the worker's
   * word, which nothing verifies. */
  updateHead: nonEmptyString.optional(),
  /** Why the daemon must replace this pod, as it found the drift, and absent while no round is
   * open: the worker answering a round is told to act on it. */
  updateReasons: nonEmptyString.optional(),
  /** How many turns of this claim ended in a provider error in a row, absent for a claim whose
   * last turn ended with none. It bounds nothing — a limit here would fail every claim of a
   * project at once in a provider outage — and the project's controller is told of it once, at
   * a run of six, so this is the only surface the number behind that telling reaches. */
  providerErrorRun: z.number().int().positive().optional(),
  /** The task the claim holds, reduced to what a reader of this body decides on and never
   * carrying its text; absent for a claim holding none. */
  pendingTask: legionPendingTaskView.optional(),
  /** When the claim's agent last began a turn and last ended one, each absent until that happens
   * and both absent for a claim whose last turn ran before the daemon recorded them. A working
   * claim whose `lastTurnStart` is hours old is wedged; an idle claim's `lastTurnEnd` is when it
   * last did anything. */
  lastTurnStart: timestamp.optional(),
  lastTurnEnd: timestamp.optional(),
  /** Why the claim's process was last found gone, and why its last start did not produce a
   * running agent — a launcher that refused the start, a failed init container, a pod that never
   * became ready. Each is absent until it happens, and the two are separate because they answer
   * separate questions: a claim that cannot start and a claim that keeps dying are different
   * faults. `lastReady` is when the claim's agent last said it could be prompted, which dates
   * both: a cause older than it is one the agent running now has already survived. */
  lastDeathCause: nonEmptyString.optional(),
  lastStartFailure: nonEmptyString.optional(),
  lastReady: timestamp.optional(),
  /** What the daemon last saw this claim's agent do; absent for a claim whose agent has neither
   * begun a turn nor made a call the daemon saw. A caller in a pod whose plugin declares a
   * contract below 57 is served the state with no such key at all (the daemon's
   * `withoutStallRecord`), so every reader takes it as optional. */
  step: legionStepView.optional(),
  /** Why the claim failed, and absent for a claim the daemon holds no cause and sentence for: one
   * that never failed, and one whose launch cleared the record. A caller in a pod whose plugin
   * declares a contract below 75 is served the state with no such key at all (the daemon's
   * `withoutRecordFields`), so every reader takes it as optional. */
  failure: legionClaimFailureView.optional(),
  /** What failed the process whose relaunch is in flight, and absent for a claim relaunching
   * after no failure: a claim that died and whose relaunch the runtime then refused reads
   * `died` here, which is what an architect deciding a retry or an escalation acts on while
   * `failure` is still empty. It is stripped with `failure` below the same contract. */
  pendingCause: legionFailureCause.optional(),
  /** The sentence the claim's latest launch try was refused with because the shared public leak
   * check could not be installed in the role's pod, beginning `the shared public leak check could
   * not be installed: `; absent once a try starts its process or fails for another reason. It
   * stands beside `failure` rather than in it: a claim that runs out of launches on it fails with
   * the cause and the sentence it would have failed with anyway, and this key says which check
   * refused it. A caller in a pod whose plugin declares a contract below 87 is served the state
   * with no such key at all (the daemon's `withoutLeakCheckCause`), so every reader takes it as
   * optional. */
  leakCheckCause: nonEmptyString.optional(),
});

/** `api.HandoffRecordView` — the Dispatch handoff record a worker's completion of its current
 * file-backed phase named: its id, and its version among its phase's records. */
const legionHandoffRecordView = z.object({
  id: nonEmptyString,
  version: z.number().int().positive(),
});

/** `api.PhaseView` — one phase worker's claim, the handoff of its current phase (the commit its
 * completion reported, the Dispatch handoff record it named), and the three counts the phase
 * carries: `pass`, the passes it has made through its stretch of the workflow, which every
 * backward move counts one of, `rounds`, the review rounds, which only a review that requested
 * changes counts, and `backwardMoves`, the counts of the moves back that made those rounds. */
const legionPhaseView = z.object({
  claim: legionClaimView,
  handoffCommit: nonEmptyString.optional(),
  handoffRecord: legionHandoffRecordView.optional(),
  pass: z.number().int().nonnegative(),
  rounds: z.number().int().nonnegative(),
  /** How many times the issue's work has gone back by each kind of move, keyed `<from>-<to>` as
   * the daemon counts the pairs (`reviewing-implementing` and the rest). Absent, never `{}`, for
   * a worker whose issue has had no move back, and absent from the whole state for a caller in a
   * pod whose plugin declares a contract below 62 (the daemon's `withoutBackwardMoves`), so every
   * reader takes it as optional. `rounds` beside it counts every round, a conflict round
   * included, which no pair counts. */
  backwardMoves: z.record(z.string(), z.number().int().positive()).optional(),
});

/** `api.PullRequestView` — the pull request as the daemon observes it from GitHub.
 * `reviewDecision` is the latest review round's decision, the one the workflow ends the round on
 * when the reviewer completes. It shows from the review that decided it until the next round
 * opens or a new generation clears it, so a request for changes stays through implementing and
 * testing, and an approval through retro and every phase after it; absent until a review in a
 * round decides. `state` is always present: a recorded pull request is open, merged or closed.
 * No CI verdict and no fix-attempt count ride here: a CI result moves no issue, so the daemon
 * keeps neither (dispatch://INFRA-714/spec R1), and `mergeState` is what says whether a check the
 * repository requires holds the head. `fixAttempts` counted the heads pushed onto a red verdict
 * until contract 59 removed the count. */
const legionPullRequestView = z.object({
  /** The repository (`owner/name`) the pull request is in, its issue's (contract 31). */
  repo: nonEmptyString,
  number: z.number().int().positive(),
  head: nonEmptyString,
  /** `open`, `merged` or `closed` (contract 36), `record.PullRequestState`'s values. */
  state: z.enum(["open", "merged", "closed"]),
  /** GitHub's own merge state for the head, in GitHub's lowercase spelling, as the daemon's last
   * read of the pull request found it (`record.MergeState`); absent until that read has run. It
   * says whether GitHub would merge the head now (`clean`) or holds it on a check or a review the
   * repository requires (`blocked`). */
  mergeState: z
    .enum(["clean", "unstable", "has_hooks", "blocked", "dirty", "behind", "draft", "unknown"])
    .optional(),
  reviewDecision: nonEmptyString.optional(),
});

/** `api.GateView` — the design gate; `approvedVersion` is null until a human approves one, and
 * the gate is open exactly when it equals `currentVersion`. */
const legionGateView = z.object({
  artifactId: nonEmptyString,
  currentVersion: z.number().int().positive(),
  approvedVersion: z.number().int().positive().nullable(),
});

/** `record.DesignStep` — one step of a deployment's design gate, each naming one document of the
 * root issue: `decisions` its decisions document, `criteria` its Verification document, `spec`
 * its primary document. */
const legionDesignStep = z.enum(GATED_STEPS);

/** `api.GatedStepView` — one step of the deployment's design gate as one issue stands in it
 * (contract 55). `artifactId` and `currentVersion` are the document its architect registered for
 * the step and that document's current version, absent until one is registered; `approvedVersion`
 * is the version a person approved, null until one does. `status` is where the step stands:
 * `unregistered` (the deployment gates it and the issue holds no record of it, so it holds the
 * tree's work), `waiting` (registered, and no person has reviewed its current version),
 * `approved` (a person approved its current version, the one status that lets the work past the
 * step) or `changes_requested` (its reviewer asked for changes at its current version, with
 * `changesReason` the reason they gave; absent under every other status). */
const legionGatedStepView = z.object({
  step: legionDesignStep,
  artifactId: nonEmptyString.optional(),
  currentVersion: z.number().int().positive().optional(),
  approvedVersion: z.number().int().positive().nullable(),
  status: z.enum(["unregistered", "waiting", "approved", "changes_requested"]),
  changesReason: nonEmptyString.optional(),
});

/** `api.SlotView` — the admission slot the issue occupies and when it took it. */
const legionSlotView = z.object({
  index: z.number().int().nonnegative(),
  admittedAt: timestamp,
  lentTo: issueKey.optional(),
});

/** `api.WaitingLineView` — where the issue stands in `admission.waiting`: `place`, 1 being first
 * in the line, and `length`, how long that list was at this read, beside the line's own fields
 * for a program that acts on the line rather than reading the sentence `reason` renders: `part`,
 * which of the line's two parts the issue waits in, and `kind`, why it waits there; `partPlace`
 * and `partLength`, its place within its own part and that part's length; `holdsSlot`, whether it
 * already holds one of the cap's slots; and what placed it — `since`, the moment its work came
 * back, for a member of the `returned` part, and `rootRank`, `ownRank` and `rootKey`, the Dispatch
 * ranks and the tree root's key, for a member of the `starting` part. It says where the issue
 * stands, not what holds it, which `admission.blocked` names.
 *
 * Every member below `length` is optional, for two readers at once: a member carries the fields of
 * its own part alone, so a returned member names no rank and a starting member no `since`; and a
 * caller below the contract that brought `partPlace` and the rest of them, contract 85, is served
 * `place` and `length` alone, with no field of theirs on the wire at all.
 *
 * `part` and `kind` are written as the enums they are, which is how `LEGION_WAIT_ANSWERS` writes
 * the acts that end a root's wait: a `z.enum` refuses a value it does not name rather than dropping
 * it, so a word a later daemon adds would refuse the whole state body here — and a daemon that adds
 * one declares a new contract for it, as the ninth answer did at contract 68, which is what keeps
 * this reader away from a body that carries it. Nothing older reaches them either: both words are
 * rendered afresh from `record.LinePart` and `record.LineKind` at each read and never stored, so no
 * retired word comes back off a row, and naming the four kinds is what lets a reader branch on
 * them. */
const legionWaitingLineView = z.object({
  place: z.number().int().positive(),
  length: z.number().int().positive(),
  part: z.enum(["returned", "starting"]).optional(),
  kind: z.enum(["returning", "answered", "queued_root", "first_phase"]).optional(),
  partPlace: z.number().int().positive().optional(),
  partLength: z.number().int().positive().optional(),
  holdsSlot: z.boolean().optional(),
  since: timestamp.optional(),
  rootRank: nonEmptyString.optional(),
  ownRank: nonEmptyString.optional(),
  rootKey: issueKey.optional(),
  reason: nonEmptyString.optional(),
});

/** `api.WhyView` — the one rendered answer an issue that is not working now carries: `state`,
 * which of the four facts holds it — its claim has stopped (`claim_stopped`), it waits in the
 * admission line (`waiting`), something holds it where it stands (`blocked`), or it stands in
 * neither the line nor the work at all (`unlisted`) — and `reason`, the sentence a person reads.
 * The projection reads the four in one order over the facts the pass already holds and renders the
 * sentence of whichever holds the issue, so an issue carries one answer and never two. `state` is a
 * `z.enum` for the reason `part` and `kind` above are, and it arrives with the line's own
 * `partPlace` and the rest of its fields at contract 85. */
const legionWhyView = z.object({
  state: z.enum(["claim_stopped", "waiting", "blocked", "unlisted"]),
  reason: nonEmptyString,
});

/** `api.CheckWakeView` — what woke a production check's latest look: `kind` names the cause
 * (`merge` for the look the close opens, `check wake` for one the checking agent's own `wake_at`
 * or the retry of a look nobody reported starts, and `bound` for the last look; a check whose
 * look was woken before the daemon stopped reading production can hold one of the causes that
 * are gone), `detail` the cause's own detail — for `check wake`, the summary the agent's report
 * gave or the retry's own words — `at` when. */
const legionCheckWakeView = z.object({
  kind: nonEmptyString,
  detail: z.string(),
  at: timestamp,
});

/** `api.CheckView` — an issue's open production check, the work the project's `production_check`
 * role takes after the merge closed the issue (contract 33, contract 54): the repository, pull
 * request and merge commit the check looks for in production (`mergeCommit` is empty for a pull
 * request merged before the daemon recorded merge commits), when it opened, the role the check
 * records as the one that works it (`role`), how many looks that role has taken and whether the
 * latest is still open, what woke the latest look (`lastWake`, absent before the first look
 * wakes), when the next look is due (`nextWakeAt`), and whether the last look has started.
 * `nextWakeAt` is whichever names that look: the moment the checking agent named for itself on a
 * pending report, the retry of a look nobody reported, or the check's own bound — the moment its
 * last look starts — where neither stands. The bound has no field of its own: `nextWakeAt` is the
 * bound whenever no agent's time is set, `openedAt` is here, and `production_check_hours` is in
 * the settings. Present on the issue record only while the check is open; absent once it closed
 * or while none was opened. */
const legionCheckView = z.object({
  repo: nonEmptyString,
  pullRequest: z.number().int().positive(),
  mergeCommit: z.string(),
  openedAt: timestamp,
  role: nonEmptyString,
  looks: z.number().int().nonnegative(),
  lookOpen: z.boolean(),
  lastWake: legionCheckWakeView.optional(),
  nextWakeAt: timestamp,
  last: z.boolean(),
});

/** `record.Answer` — the act that ended a root's wait on a person: a person's answer on the awaited
 * ask (`ask`), its retraction by Dispatch's settlement (`retracted`) or its close by someone who is
 * not a person (`closed`), a person's act on another ask (`reply`), a person's decline to answer an
 * ask of the root, the awaited one or any other (`declined`, contract 68), a session handing the
 * last question back to the agent's turn (`turn`), a person's review of the spec (`approved`,
 * `changes_requested`), or a spec version newer than the one waited on (`version`). */
export const LEGION_WAIT_ANSWERS = [
  "ask",
  "retracted",
  "closed",
  "reply",
  "declined",
  "turn",
  "approved",
  "changes_requested",
  "version",
] as const;

/** The fields every `waitingOn` shape shares: `since`, Dispatch's time of the hand-back the wait
 * began at (absent when unknown), and `answeredAt` with `answeredBy` once the wait was answered
 * (`answeredBy` names how, not always by a person), when the answered root stands in admission's
 * waiting line, among the issues whose work came back, oldest first, until the pass that resumes it. */
const legionWaitTimes = {
  since: timestamp.optional(),
  answeredAt: timestamp.optional(),
  answeredBy: z.enum(LEGION_WAIT_ANSWERS).optional(),
};

/** `api.WaitingOnView` — what an `awaiting_person` root waits on from a person: `ask`, the oldest
 * question of its tree still with the person, or `gate`, a spec version (`gateVersion` of document
 * `artifactId`) a person has not reviewed. Contract 40 carries both shapes, and a daemon writes either. */
const legionWaitingOn = z
  .discriminatedUnion("kind", [
    z.object({ kind: z.literal("ask"), ask: nonEmptyString, ...legionWaitTimes }),
    z.object({
      kind: z.literal("gate"),
      gateVersion: z.number().int().positive(),
      artifactId: nonEmptyString,
      ...legionWaitTimes,
    }),
  ])
  .refine((wait) => (wait.answeredAt === undefined) === (wait.answeredBy === undefined), {
    message: "answeredAt and answeredBy appear together",
  });

/** `api.PendingStatusWrite` — one `dispatch_status` effect the outbox has not finished, due now or
 * backing off after a failed attempt: `nextAt` is the earliest it can run, and `attempts` and
 * `lastError` say why it is still pending. An issue's writes run in the order they were made, so a
 * write waits behind an older unfinished one for the same issue, which is listed too; the list runs
 * oldest first, so it shows each issue's writes in that order. The workflow owns the payload's
 * detailed shape, so the state surface preserves it as JSON. */
const legionPendingStatusWrite = z.object({
  issue: nonEmptyString,
  payload: z.record(z.string(), z.unknown()),
  attempts: z.number().int().nonnegative(),
  nextAt: timestamp,
  lastError: z.string().optional(),
});

/** `api.StrandedClaim` — one claim nothing will wake (`workflow.Stranded`): a worker of its issue's
 * phase, or its tree's architect, in a tree that runs, named by its issue, its role and its state;
 * the issue's entry under `issues` carries the rest. Its state, role and suspension say the move
 * that brings it back (`api.StrandedClaim.Move`). A `retired` entry is a claim a close or a stop
 * retired while its tree runs: only a start of the phase it owns relaunches it, and no rule the
 * daemon runs queues that start, so nothing will wake it either. That state arrived with contract
 * 56, and a pod whose claims record a contract below it is served this report without its retired
 * entries (`api`'s `stateFor`, `withoutRetiredStranded`) rather than being made to refuse the
 * whole state over a third value its enum does not name.
 *
 * Who suspended a suspended one is not here and must never be added: a session parses this body
 * with the schema of the contract its plugin was built at, and a Kubernetes deployment relaunches
 * no stale session, so a key added here refuses the whole state in every pod still on an image
 * whose plugin predates INFRA-1048, where the answer side still refused a key it did not name.
 * The operator claim routes serve it, and no plugin calls them
 * ("no session-facing response carries suspendedBy" in legion-api.test.ts). */
const legionStrandedClaim = z.object({
  issue: issueKey,
  role: z.enum(LEGION_ROLES),
  state: z.enum(["suspended", "failed", "retired"]),
});

/** `api.HeldOutboxRow` — one due lifecycle outbox row its issue's lifecycle lane cannot run yet,
 * because another lifecycle row of the issue is running (`record.Store.HeldRows`, contract 28):
 * the lane runs an issue's lifecycle rows one at a time, in the order they were queued. `waitedSeconds`
 * counts from the moment the row became due, so a retried row's earlier backoffs are not counted as
 * waiting on its sibling; `waitsOn` is the id of the running row — or, for a role's start fenced
 * behind its issue's branch row, that branch row while it runs. */
const legionHeldOutboxRow = z.object({
  id: z.number().int().positive(),
  kind: nonEmptyString,
  issue: nonEmptyString,
  waitedSeconds: z.number().int().nonnegative(),
  waitsOn: z.number().int().positive(),
});

/** `api.WakeWaiting` — one wake the daemon's pacing is holding until its turn
 * (`worker_wakes_per_minute`, `worker_wake_burst`, contract 53): the claim it will wake, that
 * claim's issue and role, what kind of wake it is, and `dueAt`, the moment the pacing gave it,
 * which is when it runs. The kinds are the daemon's three paced lanes: `launch`, a role process it
 * is about to start, and `plugin-refresh`, the relaunch of a process whose plugin speaks an
 * earlier contract than the daemon's, which the stale-plugin lane's own gate admits (contract
 * 73), both held in the claim's machine and lost with a restart, which raises the wave
 * again through its own paths; and `catch-up`, the notice a start writes to an architect already
 * running in place of a launch, which waits on its own outbox row. `kind` is an unconstrained
 * string, so a lane a later daemon names is read rather than refused. */
const legionWakeWaiting = z.object({
  claim: nonEmptyString,
  issue: issueKey,
  role: z.enum(LEGION_ROLES),
  kind: nonEmptyString,
  dueAt: timestamp,
});

/** `api.Issue` — `workers` is keyed by a plain string, not by the role names this build knows, so
 * a role a later daemon adds is read rather than refused; the key list is held by the golden test
 * in `legion-api.test.ts`, not by this schema. A phase that has not run has no entry, so the value
 * is optional. `waitingOn` is present only in `awaiting_person`; `check` is the issue's open
 * production check, present only on a `done` issue while the check is open. */
const legionIssue = z.object({
  key: nonEmptyString,
  generation: z.number().int().nonnegative(),
  phase: z.enum(LEGION_PHASES),
  status: nonEmptyString,
  /** The key of the root of the issue's tree, equal to `key` on a root (contract 51). A child
   * holds an admission slot of its own, so neither `admission.active` nor `admission.waiting`
   * says which tree an issue belongs to; a reader tells a child from a root here, and finds the
   * members of a tree that lingers. Absent on an issue the workflow does not record (phase
   * `unrecorded`, an operator's claim), which stands in no tree, and absent from every issue of
   * the body the route serves a caller before 51 (`api.stateFor`, `withoutTrees`). */
  tree: issueKey.optional(),
  /** The repository (`owner/name`) the issue targets and that repository's default branch, taken
   * when the daemon first recorded the issue and never changed after; absent on an issue the
   * workflow does not record (phase `unrecorded`, an operator's claim) (contract 31). */
  repo: nonEmptyString.optional(),
  defaultBranch: nonEmptyString.optional(),
  /** Why a held issue is held, when its hold has one: `escalated` once its architect sent it to
   * the controller. `heldFrom` beside it is the phase the hold took the issue out of, and is
   * absent for an issue that is not held. The linger suppression is `holdReason`'s alone: the
   * reason is absent while the issue's tree lingers or is closed, until it is re-admitted, since
   * it is a decision someone must act on and a lingering tree has none; the phase is a fact of
   * the record that costs one word, so every issue at `phase: held` names the phase that
   * stopped, whatever its tree is doing. A caller in a pod whose plugin declares a contract
   * below 75 is served every issue without `heldFrom` (the daemon's `withoutRecordFields`), so
   * every reader takes it as optional. */
  holdReason: z.literal("escalated").optional(),
  heldFrom: z.enum(LEGION_WORKFLOW_PHASES).optional(),
  /** When this root's linger runs out, absent for every issue that is not a lingering root:
   * only a root carries a linger, and re-admission clears it. It is what keeps `holdReason`'s
   * own suppression readable — a held issue of a lingering tree is served no reason, so without
   * this an absent reason means either no reason or a tree that lingers, and a reader cannot
   * tell which. */
  lingerUntil: timestamp.optional(),
  /** The keys of a labelled root's Dispatch parent chain, from its parent upward, absent for
   * every issue the record holds none for: a root that ran as a tree of its own, and every
   * member of a tree. A root under an unreleased child of a live tree waits for no slot while
   * any live root stands on its chain, and this is the only surface that chain reaches. */
  ancestry: z.array(issueKey).optional(),
  architect: legionClaimView.optional(),
  workers: z.partialRecord(z.string(), legionPhaseView),
  pullRequest: legionPullRequestView.optional(),
  designGate: legionGateView.optional(),
  /** The deployment's design gate as this issue stands in it, one entry for each step the
   * deployment gates, in the order it gates them (contract 55). This daemon always writes it, and
   * it is empty for an issue that is not its tree's root, for a deployment that gates no step and
   * for an issue the workflow does not record; a daemon before contract 55 writes none, which is
   * why a reader takes it as absent rather than as an empty list. `designGate` above is unchanged
   * beside it, the `spec` step's row, which a reader that asks one question of one gate takes. */
  gatedSteps: z.array(legionGatedStepView).optional(),
  slot: legionSlotView.optional(),
  /** Where the issue stands in `admission.waiting` (contract 58): present only for an issue that
   * list names, so every issue the line does not carry has none, and a daemon before contract 58
   * writes none at all, which is why a reader takes it as optional. */
  waitingLine: legionWaitingLineView.optional(),
  /** The one rendered answer an issue that is not working now carries: which fact holds it, and
   * the sentence that says so. It sits here beside the line and never inside it, because an issue
   * whose claim has stopped may stand in that line or outside it — such a member carries this key
   * and its `waitingLine` both, and reads `claim_stopped`, the state that wins where both hold —
   * and the answer is the same answer for every issue that carries one. Present only for an issue
   * the record holds in the workflow whose sentence renders something, so an issue at work
   * carries none; a caller served the line's own `partPlace` and the fields that arrived with it
   * is served this key too, and one below contract 85 is served neither (`api.stateFor`,
   * `withoutWhyAndLineFields`). */
  why: legionWhyView.optional(),
  waitingOn: legionWaitingOn.optional(),
  check: legionCheckView.optional(),
  /** The memory group of the issue's tree: `memory` for a tree whose roles run with the project's
   * memory overlays and instructions, `none` for one that runs without them, with the project's
   * `memory.mode` in force when the group was drawn and the instant of that draw. The three are
   * absent together while nothing has drawn for the tree — a tree first opened before the setting
   * existed — and every issue of a tree carries its root's, since the draw is made once, on the
   * root, when the tree's lifecycle row is first created (contract 49). */
  memoryGroup: z.enum(["memory", "none"]).optional(),
  memoryDrawMode: z.enum(["off", "on", "random"]).optional(),
  memoryDrawnAt: timestamp.optional(),
  /** The entry the role of the issue's current phase stands on, and the entry that role was last
   * given a task of (contract 94). The daemon sends the entry's own work to a role whose given
   * number is below the entry its row names, on every admission pass and on a sweep of its own
   * each minute, so the two are equal for a role holding its work and differ for one the daemon
   * has still to reach. Both are absent together for an issue whose current phase names no phase
   * worker role — a root writing its design — and each is absent where it is 0, which is a role
   * no entry has been raised for and one the daemon holds no claim row of; a daemon before
   * contract 94 writes neither, and a caller in a pod whose plugin declares a contract below 94
   * is served the state with neither (the daemon's `withoutPhaseEntries`), which is why a reader
   * takes both as optional. */
  phaseEntry: z.number().int().nonnegative().optional(),
  givenEntry: z.number().int().nonnegative().optional(),
});

/** `api.DaemonInfo` — the running daemon: its project, its store's schema, and its boot history. */
const daemonInfo = z.object({
  project: nonEmptyString,
  schemaVersion: z.number().int().nonnegative(),
  boots: z.number().int().nonnegative(),
  firstBootAt: timestamp,
  startedAt: timestamp,
});

/** `api.ContractView` — the mark of the narrowing, beside the daemon's own block: `daemon` is the
 * contract the daemon declares and served this body at (its `DaemonAPIVersion`), and `caller` the
 * contract the claims of the caller's own pod record, absent for a reader the daemon places in no
 * pod — the operator's `legion state --json`, a tmux pane, a pod whose claims record no contract
 * — whose body is served whole and narrowed by nothing (contract 75).
 *
 * The daemon serves a caller below 75 the state with this key gone, as it strips the nine fields
 * of the decided record (`api.withoutRecordFields`): a key added to a narrowed body is exactly
 * what the narrowing exists to prevent, since the plugin that body is narrowed for reads the
 * state with a strict schema and refuses the whole of it for one key it does not name. Such a
 * caller is told nothing here and the daemon's own log names its narrowing instead, which is why
 * a body carrying no mark says only that the daemon narrowed it or that it is the state as
 * internal/projection answered it, never that the daemon is older than this plugin: the boot gate
 * and `legion probe-image` refuse that pair. */
const legionContract = z.object({
  daemon: z.number().int().positive(),
  caller: z.number().int().positive().optional(),
});

/** `api.BlockedIssue` (`record.BlockedIssue`, and `api.HeldChild` on a wave release) — one issue
 * Legion is waiting to start and the issues it waits on that are not done, in the order Dispatch
 * lists them. A blocker the daemon has not yet observed as an issue of its own counts as
 * unfinished, so `blockedBy` can name a key the state's `issues` does not. */
const legionBlockedIssue = z.object({
  issue: issueKey,
  blockedBy: z.array(issueKey),
});

/** `api.Admission` — the issue cap and the issues under it, in Dispatch rank order. Concurrency
 * is capped on issues, never on workers: there is no worker queue on this wire. Since contract 51
 * the cap counts the issues the daemon has started rather than the trees (dispatch://INFRA-980):
 * `active` is every issue holding a slot and `waiting` the admission line, children as much as
 * roots, so one tree with three started children holds three slots. An issue takes its slot when
 * the daemon starts it and keeps it until it is done or leaves the workflow, so `active` carries
 * an issue waiting on a person, one in `accepting`, one `held`, a merger at work in `merging`,
 * one in `merging` whose start a closed design gate holds, and a merged root standing in
 * `merging` while the rest of its tree runs, while an issue whose phase has left the workflow —
 * `done`, or a status Dispatch took it out of the workflow with — is in neither list. An issue
 * holding a slot is in `waiting` as well while its work comes back to it, an answered root until
 * the pass that resumes it. `trees` is the root of every live tree, sorted, which a reader needs
 * because a running tree may hold no slot at all — the controller reads it where it read
 * `active` as the running
 * trees. A daemon at this contract writes it on every state, empty rather than null, and it is
 * optional here only because that daemon leaves it off one body: a reader before 51 refuses the
 * key outright, so the route strips `trees` and the issues' `tree` for a caller in a pod whose
 * claims record a contract below 51 (`api.stateFor`, `withoutTrees`). A reader that finds it
 * absent has been served that stripped body and learns nothing about which trees run; it is
 * never a daemon older than this plugin, whom the boot gate and `legion probe-image` refuse.
 * `blocked` is every root and every child Legion is waiting to start behind a Dispatch
 * issue nobody has finished, by issue key; a blocked issue the line carries is in `waiting` too.
 * The controller names them in the day's report and takes nothing for them. */
const legionAdmission = z.object({
  cap: z.number().int().nonnegative(),
  active: z.array(nonEmptyString),
  waiting: z.array(nonEmptyString),
  trees: z.array(issueKey).optional(),
  blocked: z.array(legionBlockedIssue),
  free: z.number().int().nonnegative().optional(),
});

/** `api.ControllerLocator` — the record of the session registered as the project's controller: the
 * runtime the daemon runs under (`tmux` or `kubernetes`, its configured runtime), the session, and
 * when it registered. `external` is `true` for the operator-launched controller and for the one the
 * daemon launches itself under `controller: daemon`, whose process is a claim like any other. */
const legionControllerLocator = z.object({
  runtime: z.enum(["tmux", "kubernetes"]),
  external: z.literal(true),
  sessionId: nonEmptyString,
  registeredAt: timestamp,
  /** 0 = no contract recorded. */
  pluginContract: z.number().int().nonnegative(),
});

/** `api.AgentSecretsLoginView` — the daemon's own agent-secrets token login's current status
 * (runtime.kubernetes.agent_secrets): `state` is one of "none" (no login has finished yet),
 * "issued", or "failed" (the latest login won no credential). The daemon logs in with its own
 * ServiceAccount token, which no person approves, so there is no confirmation code (contract 22). */
const legionAgentSecretsLoginView = z.object({
  state: z.string(),
});

/** `api.CapabilityState` — one row of the deployment's capability report
 * (`capabilities.Deployment.Report`), in the table's order: `present`, `unchecked` (no probe has
 * checked an image row, or no session has reported a live row), `withheld` (the issue that withholds it, cited in
 * `detail`), `decided` (the operator's reason in `decision`) or `open`, an open deployment row
 * carrying `configLine`, the `legion.yaml` line that records a decision. The probe's own table also
 * prints `missing` and `reported`, which never reach this state (and `installed`, no status since
 * contract 25: nothing has emitted it since LEGION-629). A live row (`subagents`, `web-search`,
 * `mcp`, `repository-extensions`, `dispatch-envoy-tools`, `github`) is `present` or `open` from the
 * sessions' reports (`legionCapabilityReportView`), and carries no `configLine`; `live` is no
 * status since contract 21. `source` is, on a deployment row, `configuration` (what legion.yaml
 * alone decides) or `boot` (what boot measured); absent on every other row (contract 25). A gap is
 * reported here, never refused (contract 16). */
const legionCapabilityState = z.object({
  name: nonEmptyString,
  status: z.enum(["present", "unchecked", "withheld", "decided", "open"]),
  detail: nonEmptyString,
  decision: nonEmptyString.optional(),
  configLine: nonEmptyString.optional(),
  source: z.enum(["configuration", "boot"]).optional(),
});

/** `api.Settings` — the daemon's resolved legion.yaml with every credential, credential source and
 * credential file path left out (contract 35). Its keys are legion.yaml's own; the two the spec
 * names are typed, the rest ride as the daemon writes them, so a new legion.yaml key is no
 * contract change. `max_fix_attempts` was typed here until contract 59 removed the key, and
 * `review_round_cap` is the one cap left that blocks a pull request. */
const legionSettings = z.looseObject({
  review_round_cap: z.number().int().positive(),
  worker_boot_timeout_seconds: z.number().int().positive(),
});

/** `api.CacheView` — a repository cache as the Kubernetes runtime shows it: its repository and
 * Sandbox, its `state` (`failing` with the kubelet's `reason`, `refused` with its Service's),
 * whether its pod is Ready behind its own Service and its address then, from its last health read
 * the default branch's `head`, `lastFetchAt` and the `bytes` it serves or the read's
 * `healthError`, and, for a repository the mapping no longer names, `retainedUntil` (contract 32). */
const legionCacheView = z.object({
  repo: nonEmptyString,
  sandbox: nonEmptyString,
  state: z.enum(["starting", "cloning", "ready", "failing", "refused"]),
  reason: nonEmptyString.optional(),
  ready: z.boolean(),
  address: nonEmptyString.optional(),
  head: nonEmptyString.optional(),
  lastFetchAt: timestamp.optional(),
  bytes: z.number().int().positive().optional(),
  healthError: nonEmptyString.optional(),
  retainedUntil: timestamp.optional(),
});

/** `api.RepositoriesView` — the project's repository mapping as the daemon serves it: the owners
 * its roles' credentials span, each repository it serves (as GitHub names it, `default` the
 * project's default in Dispatch, `cache` its repository cache under the Kubernetes runtime), each
 * mapped repository it leaves out with `reason`, a GitHub App not installed on it, each served
 * repository Dispatch's GitHub App is not known to be installed on (`withoutWebhookApp`, as
 * Dispatch maps it, with `reason`: not installed, the check failed, or Dispatch does not answer
 * the check; served all the same, contract 43), and the `caches` of repositories the mapping no
 * longer names. Every list sorted and never null (contracts 31, 32). */
const legionRepositories = z.object({
  owners: z.array(nonEmptyString),
  mapped: z.array(
    z.object({
      repo: nonEmptyString,
      default: z.boolean(),
      cache: legionCacheView.optional(),
    })
  ),
  refused: z.array(z.object({ repo: nonEmptyString, reason: nonEmptyString })),
  withoutWebhookApp: z.array(z.object({ repo: nonEmptyString, reason: nonEmptyString })),
  caches: z.array(legionCacheView),
});

/** `api.ConsumerLagView` — one durable consumer's lag, read from NATS after the state's
 * transaction commits (contract 43): its `name`; `waiting`, the messages it has not handled
 * (JetStream's pending plus acknowledgement-pending); `oldestAt`, the stream time of the oldest of
 * them, and `oldestAgeSeconds` its age at the read, both null when nothing waits or that message
 * could not be read; `lastHandledAt`, when the daemon last acknowledged one of its messages or
 * terminated one as poison, null before the first; `stalled`, messages waiting and none handled for
 * 60 seconds; and `unavailable`, what NATS did not answer or refused, absent when every read
 * answered. */
const legionConsumerLag = z.object({
  name: nonEmptyString,
  waiting: z.number().int().nonnegative(),
  oldestAt: timestamp.nullable(),
  oldestAgeSeconds: z.number().int().nullable(),
  lastHandledAt: timestamp.nullable(),
  stalled: z.boolean(),
  unavailable: nonEmptyString.optional(),
});

/** `api.State`, the body of `GET /legion/v1/state`. `controllerLocator` is absent until a session
 * registers as the project's controller: with the capability `legion controller start` fetched, or,
 * under `controller: daemon`, with the boot token of the daemon's own controller launch;
 * `agentSecretsLogin` is absent when the deployment configures no broker (contract 9);
 * `capabilities` is the deployment's report, never null (contract 16); `strandedClaims` is every
 * claim nothing will wake, by issue and then role, never null (contract 23); `heldOutboxRows` is
 * every due lifecycle outbox row held behind its issue's running one, by when it came due and then
 * id, never null (contract 28); `repositories` is the project's repository mapping as the daemon
 * serves it (contract 31); `settings` is the daemon's resolved legion.yaml with no credential,
 * credential source or credential file path, required, and loose like `payload` above, so a
 * legion.yaml key added later is no contract change (contract 35); being `z.looseObject` it keeps
 * the keys it does not name, where the shapes around it drop theirs; `consumers` is the lag
 * of the daemon's durable consumers, never null, empty for a daemon with no workflow (contract 43);
 * `wakesWaiting` is every wake the daemon's pacing is holding until its turn, in claim order, never
 * null, empty at every moment outside a restart or a refresh wave (contract 53). It is optional
 * here only because a daemon at this contract leaves it off one body: a reader before 53 refuses
 * the key outright, empty as surely as naming a wake, so the route strips it for a caller in a pod
 * whose claims record a contract below 53, and for that caller alone (`api.stateFor`,
 * `withoutWakesWaiting`). A reader that finds it absent has been served that stripped body and
 * learns nothing about which wakes wait; it is never a daemon older than this plugin, whom the
 * boot gate and `legion probe-image` refuse. `claimsToRefresh` is how many claims the stale-plugin
 * lane still has to relaunch onto the daemon's plugin, a number and never null, 0 when the lane has
 * nothing left (contract 73). It is what `wakesWaiting` cannot say: that lane holds at most one
 * minute of the pacing at a time, so its waiting wakes are what it has admitted and this is what it
 * has left. It is optional for the same reason `wakesWaiting` is and no other: the route strips it
 * for a caller in a pod below 73, and for that caller alone, since a reader before 73 refuses the
 * key when it is 0 as surely as when it names a number (`api.stateFor`, `withoutClaimsToRefresh`).
 * A caller in a pod whose claims record no contract at all is served the count, as is a caller the
 * daemon can place in no pod. The two strips compose on one body: a caller below 53 is below 73 too
 * and is served neither key. `contract` is the mark of the narrowing, the two contract numbers
 * the daemon's own log line names (contract 75), optional for the same reason the other two are:
 * the route strips it for a caller in a pod below 75, and for that caller alone, since a reader
 * before 75 refuses the key (`api.stateFor`, `withoutRecordFields`). A caller in a pod whose
 * claims record no contract and a caller the daemon can place in no pod are served the mark, and
 * the shapes `internal/projection` answers before the route marks them carry none either. */
export const LegionStateResponse = z.object({
  daemon: daemonInfo,
  contract: legionContract.optional(),
  admission: legionAdmission,
  issues: z.record(z.string(), legionIssue),
  pendingStatusWrites: z.array(legionPendingStatusWrite),
  heldOutboxRows: z.array(legionHeldOutboxRow),
  strandedClaims: z.array(legionStrandedClaim),
  wakesWaiting: z.array(legionWakeWaiting).optional(),
  claimsToRefresh: z.number().int().nonnegative().optional(),
  controllerLocator: legionControllerLocator.optional(),
  agentSecretsLogin: legionAgentSecretsLoginView.optional(),
  capabilities: z.array(legionCapabilityState),
  repositories: legionRepositories,
  settings: legionSettings,
  consumers: z.array(legionConsumerLag),
});

export type LegionState = z.output<typeof LegionStateResponse>;
export type LegionIssue = LegionState["issues"][string];

/** `claim.RegisterResponse`, the body of `POST /legion/v1/claims/register`: the claim the agent
 * holds, the secret its ready and exit authenticate with, and `promptAgents`, the sorted names of
 * the task agents the role's prompts dispatch — what the session's subagents check measures its
 * discovered agents against (contract 21). Never null: a role whose prompts name none gets `[]`. */
export const LegionRegisterResponse = z.object({
  claimToken: nonEmptyString,
  tree: nonEmptyString,
  issue: nonEmptyString,
  role: z.enum(LEGION_ROLES),
  generation: z.number().int().positive(),
  secret: nonEmptyString,
  promptAgents: z.array(nonEmptyString),
});

export type LegionRegistration = z.output<typeof LegionRegisterResponse>;

/** `claim.CapabilityReport` — what a session measured of the live capability rows: when it
 * measured, how long the measuring took, and one row per capability it checked, `ok` with the fact
 * the check found in `detail`. A `detail` may be empty on the wire: a passing check with nothing
 * to add. The daemon normalises the rows (unknown names dropped, a live row not reported marked
 * open, details cut) and never refuses a report (contract 21). */
export const LegionCapabilityReport = z.strictObject({
  measuredAt: timestamp,
  elapsedMs: z.number().int().nonnegative(),
  rows: z.array(z.strictObject({ name: nonEmptyString, ok: z.boolean(), detail: z.string() })),
});

export type LegionCapabilityReportBody = z.output<typeof LegionCapabilityReport>;
export type LegionCapabilityRow = LegionCapabilityReportBody["rows"][number];

/** `claim.ReadyRequest`, the body of `POST /legion/v1/claims/ready`: the registered claim says its
 * agent has booted and can be prompted, authenticated by the registration's secret and naming the
 * launch generation it answers for. A boot's ready and a role-regain's carry no report; a claimed
 * session sends its report on a `claims/ready` of its own, from the interval it runs while it is
 * idle. A ready without a `capabilities` report reports nothing and changes no row. */
export const LegionReadyRequest = z.strictObject({
  claimToken: nonEmptyString,
  sessionId: nonEmptyString,
  secret: nonEmptyString,
  generation: z.number().int().positive(),
  capabilities: LegionCapabilityReport.optional(),
});

export type LegionReadyRequestBody = z.output<typeof LegionReadyRequest>;

/** The half of `claim.TurnOutcomeRequest` that is the turn rather than the caller: `sequence` is
 * the producer's own count of the turns it has ended at `generation`, from 1, and the daemon
 * keeps the report under the pair, so a report that arrives late or out of order lands in its own
 * place and the run of refused turns it derives is the same either way. `message` is what the
 * provider said about a refusal, and a clean turn carries none. */
const turnOutcomeFields = {
  generation: z.number().int().positive(),
  sequence: z.number().int().positive(),
  outcome: z.enum(["clean", "refused"]),
  message: z.string().optional(),
};

/** `claim.AgentCaller`'s report: the producer inside the agent's own session — pi-legion's
 * `agent_end` hook for an Oh My Pi session — which holds the secret its registration was issued
 * and names the claim with it, as the ready does. */
export const LegionTurnOutcomeAgentRequest = z.strictObject({
  claimToken: nonEmptyString,
  sessionId: nonEmptyString,
  secret: nonEmptyString,
  ...turnOutcomeFields,
});

/** `claim.BootCaller`'s report: the producer beside the agent rather than inside it, which holds
 * no secret — `legion claude-bridge`, whose pane's registration is claude-envoy's, inside Claude
 * Code. It presents the launch's own boot token, which names the same claim, and the route holds
 * it to that launch: a token of a launch the claim has moved past, and a report for any other
 * generation, are refused. */
export const LegionTurnOutcomeBootRequest = z.strictObject({
  bootToken: nonEmptyString,
  ...turnOutcomeFields,
});

/** `claim.TurnOutcomeRequest`, the body of `POST /legion/v1/claims/turn-outcome`: how one turn of
 * the claim's agent ended, reported by the producer that ended it, under exactly one of the two
 * caller identities above. A body that names both, or neither, is refused
 * (`claim.TurnOutcomeRequest.Caller`). */
export const LegionTurnOutcomeRequest = z.union([
  LegionTurnOutcomeAgentRequest,
  LegionTurnOutcomeBootRequest,
]);

export type LegionTurnOutcomeRequestBody = z.output<typeof LegionTurnOutcomeRequest>;

/** `claim.TurnOutcomeResponse`, the answer to a reported turn: `refusedRun` is the run of refused
 * turns the claim's reports now end with, and `failed` whether this report took that run to
 * `launch_failure_limit` and failed the claim. The producer acts on neither — its turn is over —
 * and logs them, which is where an operator sees the count that failed a claim being made. */
export const LegionTurnOutcomeResponse = z.object({
  refusedRun: z.number().int().nonnegative(),
  failed: z.boolean(),
});

export type LegionTurnOutcome = z.output<typeof LegionTurnOutcomeResponse>;

/** `claim.ReadyForUpdateRequest`, the body of `POST /legion/v1/claims/ready-for-update`: the
 * worker's answer to its pod's update task, which says it has pushed everything it wants kept and
 * its process may be replaced. `round` is the round the worker answers for, as the update task it
 * read named it: the daemon takes the answer only for the round its claim stands in, and refuses
 * one naming any other round with 409 and a sentence naming both. `head` is the commit the worker
 * reports as the head of `legion/<issue>@origin`, empty or absent when the branch is not there (a
 * planner before its first push); the daemon records it and verifies nothing of it. */
export const LegionReadyForUpdateRequest = z.strictObject({
  claimToken: nonEmptyString,
  sessionId: nonEmptyString,
  secret: nonEmptyString,
  generation: z.number().int().positive(),
  round: nonEmptyString,
  head: z.string().optional(),
});

export type LegionReadyForUpdateRequestBody = z.output<typeof LegionReadyForUpdateRequest>;

/** `claim.SessionActivity` — what a live role's session is doing, read at `at`: whether work of
 * its own is running in the background and of what kinds (`async-job`, `subagent`,
 * `claude-background-task`; sorted and de-duplicated, so a hold's reason reads the same every
 * time), and when the phase reminder armed in it is due, null when no armed reminder is still
 * live. Whether the phase is open is the daemon's own reading of the record, not the session's
 * to state. */
export const LegionSessionActivity = z.strictObject({
  at: timestamp,
  backgroundWork: z.strictObject({ running: z.boolean(), kinds: z.array(nonEmptyString) }),
  phase: z.strictObject({ nextReminderAt: timestamp.nullable() }),
});

export type LegionSessionActivityBody = z.output<typeof LegionSessionActivity>;

/** `claim.ActivityRequest`, the body of `POST /legion/v1/claims/activity`: a live role's session
 * reporting what it is doing, after each of its turns and whenever the report's content changes,
 * so the daemon can decide whether the session's pod may be released. It is authenticated by the
 * registration's secret, as `claims/ready` is, and names no generation: it decides nothing about
 * the claim, and a relaunch mints a secret of its own, so the secret is the fence. A claim with
 * no report after its last turn is never released, which is what makes a host that cannot report
 * safe. */
export const LegionActivityRequest = z.strictObject({
  claimToken: nonEmptyString,
  sessionId: nonEmptyString,
  secret: nonEmptyString,
  activity: LegionSessionActivity,
});

export type LegionActivityRequestBody = z.output<typeof LegionActivityRequest>;

/** `api.ReleaseRecord` — one issue pod the daemon released: the run it released, the roles whose
 * claims it suspended (sorted, as the store holds them), when the release completed, and the size
 * of the git bundle of unpushed work it saved, zero when the workspace held none. */
const legionReleaseRecord = z.object({
  id: z.number().int().positive(),
  issue: issueKey,
  generation: z.number().int().nonnegative(),
  roles: z.array(nonEmptyString),
  at: timestamp,
  bundleBytes: z.number().int().nonnegative(),
});

/** `api.ResumeRecord` — one wake of a released pod and what it cost: the release it ended (0 when
 * no release row is recorded for it), the role it was for, what woke it, when the wake reached the
 * daemon, and `podReadyAt` and `firstTurnAt`, each absent until it is observed. `stuck` is a wake
 * that went ten minutes with no turn of the woken role. */
const legionResumeRecord = z.object({
  id: z.number().int().positive(),
  release: z.number().int().nonnegative(),
  issue: issueKey,
  role: nonEmptyString,
  kind: nonEmptyString,
  at: timestamp,
  podReadyAt: timestamp.optional(),
  firstTurnAt: timestamp.optional(),
  stuck: z.boolean(),
});

/** `api.HoldRecord` — one streak of the idle rule keeping a pod past the idle period, with the
 * rule's own name for what held it (`workflow.IdleHold`) and the detail that word leaves out —
 * the kinds a background-work hold's session reported — left out where there is none. A reason
 * the rule read again on the next pass is the same record: `firstAt` is the streak's first pass,
 * `at` its newest and `passes` how many it has made, so a pod held for days is one row rather
 * than one a minute. The streak ends when the pod is released, when the rule reads another
 * reason for it, or when a pass holds it for no reason worth a row, and a hold after that is a
 * record of its own: the records of a window are the streaks of that window, not one row per
 * pod and reason for all time. The daemon records no row for a pod that is simply not idle
 * yet. */
const legionHoldRecord = z.object({
  id: z.number().int().positive(),
  issue: issueKey,
  reason: nonEmptyString,
  detail: nonEmptyString.optional(),
  firstAt: timestamp,
  at: timestamp,
  passes: z.number().int().positive(),
});

/** `api.ReleaseRecordsRequest`, the body of `POST /legion/v1/claims/release-records`: the claim
 * the reading pod holds, that claim's secret, and the start of the window it reads. The caller is
 * an issue pod, which holds its claim credential and no operator token, so the claim secret
 * authenticates it as `claims/ready` is authenticated — and it is a POST rather than a GET with a
 * query because that secret is a credential, which a URL carries into every proxy and access log
 * between the pod and the daemon. `since` is a `LegionInstant`, not a `timestamp`: the session
 * sends it, the daemon decodes it with `time.Parse(time.RFC3339, …)`
 * (`internal/api/claims.go`), which refuses a time with no seconds, and so does this. */
export const LegionReleaseRecordsRequest = z.strictObject({
  claimToken: nonEmptyString,
  secret: nonEmptyString,
  since: LegionInstant,
});

export type LegionReleaseRecordsRequestBody = z.output<typeof LegionReleaseRecordsRequest>;

/** `api.ReleaseRecordsResponse`, the body `POST /legion/v1/claims/release-records` answers: every
 * release, resume and hold of the project recorded at or after the `since` the caller names,
 * oldest first. Every list is present: a window with nothing in it is three empty lists, never
 * three absent ones. */
export const LegionReleaseRecordsResponse = z.object({
  releases: z.array(legionReleaseRecord),
  resumes: z.array(legionResumeRecord),
  holds: z.array(legionHoldRecord),
});

export type LegionReleaseRecords = z.output<typeof LegionReleaseRecordsResponse>;

/** `api.ControllerRegisterResponse`, for a session that registered as the project's controller
 * (the `bootToken` of `POST /legion/v1/claims/register` is the secret `legion controller start`
 * fetched, or a daemon-launched controller's own boot token): the project's controller role token,
 * the role `controller`, a generation — the capability's for the operator's controller, the
 * launch's for the daemon's own, which its `claims/ready` names — and the secret its controller
 * grants authenticate with. No tree, no issue. */
export const LegionControllerRegisterResponse = z.object({
  claimToken: nonEmptyString,
  role: z.literal("controller"),
  generation: z.number().int().positive(),
  secret: nonEmptyString,
});

export type LegionControllerRegistration = z.output<typeof LegionControllerRegisterResponse>;

/** `api.ControllerSecretResponse`, the body of `POST /legion/v1/controller/secret`: the controller
 * capability the operator's bearer bought, and the design-gate steps the project gates
 * (`gates.design`), in the order it gates them, which `legion controller start` tells the
 * controller (contract 55). The list is empty for a deployment that gates no step and is never
 * absent. The CLI reads it; the plugin never does. */
export const LegionControllerSecretResponse = z.object({
  secret: nonEmptyString,
  designGateSteps: z.array(legionDesignStep),
});

/** `api.ControllerCredentialResponse`, the answer of `POST /legion/v1/controller/github-credential`:
 * the review App's read-only token for every GitHub owner of the project, each rendered as the two
 * files gh reads (`ghconfig.Rendered`) and keyed by lowercased owner, the owner whose directory
 * `GH_CONFIG_DIR` names, and the soonest of the tokens' expiries. `legion controller start` writes
 * them under `<state_dir>/gh/<owner>` and refreshes them every minute. The plugin never reads it. */
export const LegionControllerCredentialResponse = z.object({
  owners: z.record(
    nonEmptyString,
    z.object({
      hosts: nonEmptyString,
      config: nonEmptyString,
    })
  ),
  default: nonEmptyString,
  app: z.enum(["implement", "review"]),
  expiresAt: timestamp,
});

/** Claim routes refuse with only a sentence; credential and workflow routes add a stable code.
 * One object with an optional `code`, not a union of a sentence-only shape and a coded one: now
 * that the answer side ignores unknown keys, a union matches its first member and drops `code`,
 * and `daemonUnreachable` in `packages/pi-shared/src/daemon-client.ts` reads a 502 with no code as
 * a restarting daemon and retries it for ten minutes, so a coded refusal must keep its code. */
export const LegionErrorResponse = z.object({
  error: nonEmptyString,
  code: nonEmptyString.optional(),
});

/** `api.DeliveryView` — the claim's pending task; `deliveredAt` is the latest acknowledgement of
 * this task's prompt, and with it whether the agent may have read the task (a refusal of that
 * prompt clears it, since an agent that refused it never read it), and `confirmedAt` the turn a
 * send started, each absent until it happens. `phase` is the issue phase the task was queued for, which is what
 * says whether it is still the work to do; a task of no phase — an operator's own, an
 * architect's — carries none. */
const legionDeliveryView = z.object({
  id: nonEmptyString,
  task: nonEmptyString,
  phase: z.enum(LEGION_WORKFLOW_PHASES).optional(),
  queuedAt: timestamp,
  deliveredAt: timestamp.optional(),
  confirmedAt: timestamp.optional(),
  interrupted: z.literal(true).optional(),
  /** How many sends of this task came back without reaching a turn, when the last of them failed
   * and the error it failed with. The three are a diagnosis record, not a budget: the budget is
   * `budgets.promptFailures` and `budgets.promptRetires`, which only a charged failure spends,
   * while these count every send that failed that way, charged or not — a send lost to the
   * transport costs nothing and is retried for ever, which is the stall they exist to name. A
   * prompt the agent acknowledged and then refused is charged and read in the budget instead. A
   * send that reaches a turn clears all three, so a task no send has failed for carries none. */
  sendFailures: z.number().int().nonnegative().optional(),
  lastSendFailureAt: timestamp.optional(),
  lastSendError: nonEmptyString.optional(),
});

/** Who a suspension is held for (`supervise.SuspendedBy`). `unknown` is a suspension recorded
 * before migration 0038; `released` is one whose idle issue pod the daemon released, which keeps
 * the claim's phase and its pending task until the next wake for its role; only an `operator`
 * one holds against the daemon's rules. */
const legionSuspendedBy = z.enum(["unknown", "operator", "daemon", "exit", "released"]);

/** `api.OperatorClaim`, the body of the operator routes that act on one claim (`POST
 * /legion/v1/operator/claims`, `…/{token}/deliver|suspend|resume|stop|retry|forget-session|close`):
 * the claim as its supervisor holds it, but its hashes. `forgottenSession` and
 * `forgottenSessionFile` appear only in a successful `forget-session` response, and
 * `phaseNotRestarted` only in a `retry` that asked for a fresh session. */
export const LegionOperatorClaimResponse = z.object({
  token: nonEmptyString,
  tree: nonEmptyString,
  issue: nonEmptyString,
  role: z.enum(LEGION_ROLES),
  generation: z.number().int().nonnegative(),
  state: z.enum(LEGION_CLAIM_STATES),
  session: z.string(),
  sessionFile: z.string(),
  forgottenSession: nonEmptyString.optional(),
  forgottenSessionFile: nonEmptyString.optional(),
  /** What the request did to the claim that the claim itself does not record, on a `retry` that
   * asked for a fresh session (`{"freshSession":true}`) and nowhere else: the claim is back in a
   * new session owing no task and the workflow refused the restart that would have handed it
   * one, or the request stopped the claim and a step that would have brought it back — the
   * forget of its session, or its start — was refused, and the sentence names the state that
   * left the claim in. Either rides a successful answer, because the request changed the claim;
   * absent when the claim came back and nothing else is to be said. */
  phaseNotRestarted: nonEmptyString.optional(),
  locator: legionLocator.optional(),
  budgets: z.object({
    launchFailures: z.number().int().nonnegative(),
    deaths: z.number().int().nonnegative(),
    promptFailures: z.number().int().nonnegative(),
    promptRetires: z.number().int().nonnegative(),
  }),
  uncertainStreak: z.number().int().nonnegative(),
  pending: legionDeliveryView.optional(),
  /** Who suspended the claim while it is suspended (`legionSuspendedBy`); absent otherwise. */
  suspendedBy: legionSuspendedBy.optional(),
  /** A suspension the claim's machine holds for the agent's turn to end; absent when none is. */
  suspensionHeld: z.literal(true).optional(),
  /** Who asked for that held suspension; absent when none is held. The store writes none of
   * these three, so the state route would serve a constant for each and this route, which reads
   * the live machine, is the one that carries them. */
  suspensionHeldBy: legionSuspendedBy.optional(),
  /** The claim is working because its session is being compacted rather than because a turn is;
   * absent otherwise. */
  compacting: z.literal(true).optional(),
  /** The runtime's last report of the process this claim still records found it gone; absent for
   * a claim whose last report found it running and for one that records no process. */
  processGone: z.literal(true).optional(),
  /** 0 = no contract recorded. */
  pluginContract: z.number().int().nonnegative(),
  /** The update round the claim is in, absent while none is open: `pod:<pod uid>`, the drift
   * round every claim of that pod shares, or `plugin:<daemon contract>`, this claim's own. Once
   * the worker has answered `ready_for_update`, the daemon holds the claim's next task until the
   * round ends and the process is replaced (contract 46). */
  updateRound: nonEmptyString.optional(),
  /** The commit the worker reported as its issue branch's head at origin when it answered the
   * round; absent until it answers, and for a worker that reported none. It is the worker's
   * word, which nothing verifies. */
  updateHead: nonEmptyString.optional(),
  /** When the claim's agent last began a turn and last ended one, each absent until that happens
   * and both absent for a claim whose last turn ran before the daemon recorded them. */
  lastTurnStart: timestamp.optional(),
  lastTurnEnd: timestamp.optional(),
  /** Why the claim's process was last found gone, why its last start did not produce a running
   * agent, and when its agent last said it could be prompted; each absent until it happens. The
   * two causes answer separate questions and never overwrite each other, and `lastReady` dates
   * them. `legion claims list --json` serves them. */
  lastDeathCause: nonEmptyString.optional(),
  lastStartFailure: nonEmptyString.optional(),
  lastReady: timestamp.optional(),
  /** What the daemon last saw this claim's agent do; absent for a claim whose agent has neither
   * begun a turn nor made a call the daemon saw. These routes are the only surface a
   * controller's record reaches: the state puts the controller's claim on no issue, so no claim
   * view of it carries one. */
  step: legionStepView.optional(),
  /** Why the claim failed, and absent for a claim the daemon holds no cause and sentence for: one
   * that never failed, and one that has been given another run since, which is what clears the
   * record. It stands on a claim an operator retired after it failed, which is what lets the seat
   * reading the listing say why that worker is gone. */
  failure: legionClaimFailureView.optional(),
  /** The sentence the claim's latest launch try was refused with because the shared public leak
   * check could not be installed in the role's pod; absent once a try starts its process or fails
   * for another reason. These routes are the only surface the controller's claim reaches, so the
   * controller's cause is read here alone. */
  leakCheckCause: nonEmptyString.optional(),
});

export type LegionOperatorClaim = z.output<typeof LegionOperatorClaimResponse>;

/** `api.PodUpdateRounds` — what the daemon's last pod-update sweep decided under the project's
 * two limits on how fast one pod template change reaches the fleet. `max_open_rounds` and
 * `rounds_per_minute` are `pod_update_max_open_rounds` and `pod_update_rounds_per_minute` as the
 * daemon's configuration holds them, each `null` for a key `legion.yaml` does not set, which is
 * no limit rather than a limit of none. `in_round` is every issue that held one of the project's
 * open-round slots at that sweep, and the two waiting lists the drifted issues each limit held
 * back there, an issue both limits held back being under `waiting_on_max_open_rounds` alone.
 * Every list is in issue-key order and is an array, empty rather than absent. `swept` is false
 * for a daemon whose first sweep has not run, where the rest states nothing. */
const legionPodUpdateRounds = z.object({
  swept: z.boolean(),
  max_open_rounds: z.number().int().nullable(),
  rounds_per_minute: z.number().int().nullable(),
  in_round: z.array(nonEmptyString),
  waiting_on_max_open_rounds: z.array(nonEmptyString),
  waiting_on_rounds_per_minute: z.array(nonEmptyString),
});

/** `api.OperatorClaims`, the body of `GET /legion/v1/operator/claims`, in token order, with what
 * the daemon's last pod-update sweep decided beside the claims. */
export const LegionOperatorClaimsResponse = z.object({
  claims: z.array(LegionOperatorClaimResponse),
  pod_update_rounds: legionPodUpdateRounds,
});

/** The credential grant body from `internal/api/credentials.go`. */
export const LegionGrantResponse = z.object({
  grantId: nonEmptyString,
  expiresAt: timestamp,
});
export type LegionGrant = z.output<typeof LegionGrantResponse>;

/** Every completed fact route returns an intentional empty JSON object. The shape names no field,
 * so it accepts any JSON object and refuses a body that is not one (an array, a string); a field
 * the daemon adds to a fact route is caught by the golden drift test in `legion-api.test.ts`,
 * where each of the eight fact-route goldens is `{}`, not at runtime. */
export const LegionEmptyResponse = z.object({});
/** `api.HandoffCompleteResponse`: an empty object. The merger merges and the daemon merges
 * nothing, so no completion has anything to answer with. */
export const LegionHandoffCompleteResponse = z.object({});
/** `api.WaveReleaseResponse`. A release is accepted whatever the children's links say, so
 * `released` is every issue the request named, each once, in the order the request first named it,
 * and the architect is never asked to send the request again; `started` and `held` then say what
 * the release did with each of them. A child neither started nor held — one the daemon does not
 * record, or one still admitted that waits on nothing — is in neither list. */
export const LegionWaveReleaseResponse = z.object({
  released: z.array(nonEmptyString),
  started: z.array(issueKey),
  held: z.array(legionBlockedIssue),
});

/** `api.GrantRequest`, the session form that mints one short-lived credential grant: it serves every
 * redemption for sixty seconds while its claim holds the registration that minted it. */
export const LegionGrantRequest = z.strictObject({
  sessionId: nonEmptyString,
  secret: nonEmptyString,
  tree: nonEmptyString,
  issue: nonEmptyString,
});

/** `api.GrantRequest`, the controller-session form: the session registered with the current
 * controller capability and its registration's secret mint one controller grant. */
export const LegionControllerGrantRequest = z.strictObject({
  sessionId: nonEmptyString,
  secret: nonEmptyString,
});

/** `api.HandoffCompleteRequest`, the observation one worker reports to the workflow: a phase
 * completion, or the checking role's report on a look of its closed issue's production check.
 * A file-backed phase (planning, implementing, testing, reviewing) ends with a handoff record on
 * the Dispatch issue, which the worker writes with `dispatch handoff-write` under its own session;
 * its completion names it, `handoffId` and `handoffVersion`, which the daemon reads back from
 * Dispatch and refuses when it is missing (`HANDOFF_RECORD_REQUIRED`), unknown
 * (`HANDOFF_RECORD_NOT_FOUND`), unreadable (`DISPATCH_READ_FAILED`), of another issue, phase or
 * version (`HANDOFF_RECORD_MISMATCH`), or written by another session than the claim's
 * (`HANDOFF_SESSION_MISMATCH`). `commit` is the implementer's (its pushed tip for an implementing
 * round, the commit it stands on for retro) and the head the merger merged; the planner, the tester
 * and the reviewer report none, and a check report carries neither (the look pushed nothing), so
 * all three are optional here, and the daemon requires each where it applies. `verdict` is `pass`
 * or `fail` for the tester's completion, `pass`, `pending` or `fail` for a check report
 * (`CHECK_VERDICT_REQUIRED`), and the empty string for every other completion; it stays a string
 * on the wire, as the Go field is. `wakeAt` is the check report's alone (contract 54): when the
 * checking agent asks to be woken for its next look, which a pending report on a look that is not
 * the check's last must name (`CHECK_WAKE_REQUIRED`) and which no other verdict and no phase
 * completion may carry (`CHECK_WAKE_UNEXPECTED`). It is a `LegionInstant`, not a `timestamp`:
 * the daemon's decode of it refuses a time with no seconds, and so does this. */
export const LegionHandoffCompleteRequest = z.strictObject({
  grantId: nonEmptyString,
  summary: nonEmptyString,
  verdict: z.string(),
  commit: nonEmptyString.optional(),
  handoffId: nonEmptyString.optional(),
  handoffVersion: z.number().int().positive().optional(),
  wakeAt: LegionInstant.optional(),
});

/** `api.IssueStatusRequest`, the controller's explicit board-status write. */
export const LegionIssueStatusRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  status: z.enum(["todo", "backlog", "icebox"]),
});

/** `api.GateRegisterRequest`, the architect's current Dispatch document approval target, for one
 * step of the deployment's design gate. `step` is the step the document is for; a deployment
 * that gates one step alone takes a request that leaves it out, and one that gates several
 * refuses it (`GATE_STEP_REQUIRED`). */
export const LegionGateRegisterRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  artifactId: z.uuid(),
  version: z.number().int().positive(),
  step: legionDesignStep.optional(),
});

/** `api.WaveReleaseRequest`, the architect's selected child issues. */
export const LegionWaveReleaseRequest = z.strictObject({
  grantId: nonEmptyString,
  issues: z.array(nonEmptyString),
});

/** `api.PhaseBackwardRequest`, a request to return an issue to an earlier phase: a phase worker's
 * on its own issue, or the tree architect's on an issue it owns, from `accepting`. An architect's
 * grant serves its whole tree, so its request names `issue` (required for that grant); a phase
 * worker's grant names its own issue, so `issue` may be left out and, when given, must be the
 * grant's (`ISSUE_NOT_GRANTS`). */
export const LegionPhaseBackwardRequest = z.strictObject({
  grantId: nonEmptyString,
  to: z.enum(LEGION_PHASE_BACKWARD_TARGETS),
  reason: nonEmptyString,
  issue: nonEmptyString.optional(),
});

/** `api.PhaseRetryRequest`, the tree architect's retry-or-escalate decision for a held issue.
 * `reason` says why the decision was made, and both decisions carry it: a retry writes it into
 * the task the restarted role is given and into the daemon's own log line for the release, and
 * an escalation sends it to the controller in the held notice, beside the cause that hold
 * already names. A request that names none is unchanged: the restarted role is told the
 * daemon's own words, and the escalation's notice carries no such field. */
export const LegionPhaseRetryRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  decision: z.enum(["retry", "escalate"]),
  reason: nonEmptyString.optional(),
});

/** `api.EscalateRequest`, the architect's report to the controller. */
export const LegionEscalateRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  reason: nonEmptyString,
});

/** `api.SignOffRequest`, the owning architect's acceptance of an issue in `accepting`, which moves
 * it to `retro`; the daemon refuses it in every other phase (`SIGNOFF_OUTSIDE_ACCEPTING`). Since
 * contract 51 it is also refused while an issue beneath that one is still in the workflow
 * (`SIGNOFF_CHILD_IN_WORKFLOW`, naming them): children are accepted and merged before their
 * parent, whose own head proves their merged work (dispatch://INFRA-980). */
export const LegionSignOffRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
});

/** `api.RootCloseRequest`, a root architect's close of its tree before the tree's first phase starts,
 * with the reason the daemon posts on the issue. */
export const LegionRootCloseRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  reason: nonEmptyString,
});

/** `api.ChildRequest`, the architect's park_child or rerun_child of one child of its tree. */
export const LegionChildRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
});
