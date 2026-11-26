import { z } from "zod";
import { goBlankPattern } from "./go-blank";
import {
  DISPATCH_KEY_PATTERN,
  GATED_STEPS,
  LEGION_PLAN_RATING_LEAST,
  LEGION_PLAN_RATING_MOST,
  LEGION_PLAN_REASON_MAX,
  LEGION_ROLES,
} from "./legion-roles";

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
/** A Dispatch priority as the daemon serves it: 0 for P0 to 3 for P3, or null for none (contract
 * 250), absent where the daemon serves the caller no such key. */
const dispatchPriority = z.number().int().min(0).max(3).nullable().optional();

/** The RFC 3339 instant a session sends: seconds required, the fraction optional, `Z` or an
 * offset required, as Go's `time.Time` decode requires them. It is the stricter of the two: it
 * refuses a few strings that decode takes (`+24:00`, a comma before the fraction). `timestamp`
 * above takes `2026-11-06T10:00Z`, which that decode rejects, so a plugin that validated a request
 * field against it would send a body the daemon answers with its generic invalid-body error rather
 * than the field's own guidance. The plugin reads this same schema for the request times
 * `wake_at`, `check_opened_at` and `read_release_records`' `since`
 * (`packages/pi-shared/src/legion-phase-ops.ts`). Only a field a session sends reads it: every
 * other field here is the daemon's own output, which Go always writes with seconds. */
export const LegionInstant = timestamp.regex(
  /^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(\.\d+)?(Z|[+-]\d{2}:\d{2})$/,
  { error: "an RFC 3339 time with seconds, as 2026-11-02T09:00:00Z is" }
);

/** `api.Phase` — the state an admitted issue sits in, in the transition table's own order. Not
 * the role working it: `workers` is keyed by role (`LEGION_ROLES`). `awaiting_person` is a root
 * that keeps its admission slot while its architect waits on a person's answer (`waitingOn` on
 * the issue record). `awaiting_children` is a parent that an open Dispatch child of it holds: no
 * phase of a parent starts while such a child stands under it, so the daemon moved it here in
 * place of a phase start, or stopped its phase under way. It keeps no slot, shows `in_progress`,
 * and comes back to the start it held when no open child is left, a held `testing`, `reviewing`
 * or `accepting` as the first step of its walk after the implementer's (`testing` on the default
 * steps) unless its pull request merged meanwhile. `accepting` is the architect's acceptance,
 * where the issue's task places its `acceptance:owner` step (in the default steps, between the
 * reviewer's approval and retro): no worker runs it. The merger merges in `merging`, and the
 * daemon closes the issue when it reads that merge (`done`); the production check after the close
 * is no phase (`check` on the issue record). */
export const LEGION_WORKFLOW_PHASES = [
  "admitted",
  "awaiting_person",
  "awaiting_children",
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
    // The union's discriminator. A body without it matches no member, so `.optional()` here
    // would state what the parse does not do, and no plugin code reads it, so
    // `LEGION_ANSWER_READS` names it nowhere.
    runtime: z.literal("tmux"),
    claim: nonEmptyString.optional(),
    incarnation: nonEmptyString.optional(),
    tmux: z
      .object({ window: nonEmptyString.optional(), pane: nonEmptyString.optional() })
      .optional(),
  }),
  z.object({
    runtime: z.literal("sandbox"),
    claim: nonEmptyString.optional(),
    incarnation: nonEmptyString.optional(),
    sandbox: z
      .object({
        namespace: nonEmptyString.optional(),
        name: nonEmptyString.optional(),
        podUid: nonEmptyString.optional(),
        container: nonEmptyString.optional(),
        generation: z.number().int().positive().optional(),
      })
      .optional(),
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
  measuredAt: timestamp.optional(),
  incarnation: z.string().optional(),
  ok: z.array(nonEmptyString).optional(),
  open: z
    .array(z.object({ name: nonEmptyString.optional(), detail: nonEmptyString.optional() }))
    .optional(),
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

/** The two fields a wake the daemon's pacing holds carries: what kind of wake it is, and
 * `dueAt`, the pacing's estimate at the read of the moment it lets the wake out, as its queues
 * stand (contract 193; entry 20261125211005). A change to the queues or to the pace can move that
 * moment earlier or later, so two reads can name two moments: for example, a wake ahead of it in
 * its own queue that leaves moves it earlier, and a wake that joins and is let out ahead of it
 * moves it later. The claim view's `wake` and `legionWakeWaiting` below carry the same wake, so
 * both spread this one object and cannot drift apart in type. Both fields are optional here, since
 * no plugin code reads the claim view's `wake`; `legionWakeWaiting` requires `dueAt` over the
 * spread, because the controller's `read_state` summary reads it (LEGION_ANSWER_READS). The kind
 * is read as a plain string, so a lane a later daemon adds is read rather than refused. */
const legionPacedWake = {
  kind: nonEmptyString.optional(),
  dueAt: timestamp.optional(),
};

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
  "cyber_policy",
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
  cause: legionFailureCause.optional(),
  detail: z.string().optional(),
  at: timestamp.optional(),
  attempts: z
    .object({
      launchFailures: z.number().int().nonnegative().optional(),
      deaths: z.number().int().nonnegative().optional(),
      promptFailures: z.number().int().nonnegative().optional(),
      promptRetires: z.number().int().nonnegative().optional(),
    })
    .optional(),
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
  queuedAt: timestamp.optional(),
  sendFailures: z.number().int().positive().optional(),
  lastSendError: nonEmptyString.optional(),
});

/** `api.ClaimView` — `session` is empty until the claim's agent registers one, `locator` is
 * absent while no process runs (queued, suspended, failed, retired), `capabilities` is absent
 * until a session of the claim reports on `claims/ready` (the controller's never does), and
 * `failure` is absent for a claim the daemon holds no cause and sentence for. */
const legionClaimView = z.object({
  session: z.string().optional(),
  state: z.enum(LEGION_CLAIM_STATES),
  locator: legionLocator.optional(),
  capabilities: legionCapabilityReportView.optional(),
  /** The run of the last confirmed delivery this claim retired — the run it goes on working
   * until a delivery of another run replaces it — and absent for a claim that has retired no
   * confirmed task of a run. Read against the issue's own `generation` it says whether the work
   * the claim serves is of this generation. */
  servingGeneration: z.number().int().positive().optional(),
  /** 0 = no contract recorded. */
  pluginContract: z.number().int().nonnegative().optional(),
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
  /** Where the claim's update round stands, read from the claim's own columns (contract 193):
   * `none` when no round is open, `open` when a round is open and its ask has reached no session
   * of the worker's role yet (the reading is the mark alone: whether and when the daemon asks the
   * claim again is the daemon's to decide, and a claim it does not ask keeps reading `open`),
   * `asked` once the round's ask reached a session of the worker's role, and `answered` once the
   * worker answered. A reader takes this rather than spelling the same reading out of
   * `updateRound` and `updateHead`, which cannot tell a worker that answered with no head from
   * one nobody asked. A caller in a pod whose plugin declares a contract below 193 is served the
   * state with no such key at all (the daemon's `withoutClaimRoundReport`), so every reader
   * takes it as optional. */
  updateState: z.enum(["none", "open", "asked", "answered"]).optional(),
  /** How many turns of this claim ended in a provider error in a row, absent for a claim whose
   * last turn ended with none. It bounds nothing — a limit here would fail every claim of a
   * project at once in a provider outage. At a run of six the daemon sends the `provider-errors`
   * notice once (`providerErrorRunReached` in `packages/daemon/internal/daemon/workflow.go` says to
   * whom), so this key is the only surface that shows the count before or after that notice. */
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
   * faults. `lastReady` is when the claim's agent last said it could be prompted. `lastDeathAt`
   * and `lastStartFailureAt` (contract 256) are when the daemon recorded the cause beside each:
   * absent when that cause is, and beside a cause a daemon before 256 recorded, which wrote no
   * time, except after a rollback (the daemon's migration 20261124073512's header); a cause
   * recorded again with the same text carries its new time. `lastDeathAt` dates the
   * death: one before `lastReady` is a death the agent running now has survived. The daemon drops
   * the start failure, with its time, when the agent says it is ready, so one it records is from
   * after `lastReady`; a claim an older daemon left waiting can still carry one from before, with
   * no time. A caller in a pod whose plugin declares a contract below 256 is served neither time
   * (the daemon's `withoutCauseTimes`). */
  lastDeathCause: nonEmptyString.optional(),
  lastDeathAt: timestamp.optional(),
  lastStartFailure: nonEmptyString.optional(),
  lastStartFailureAt: timestamp.optional(),
  lastReady: timestamp.optional(),
  /** What the daemon last saw this claim's agent do; absent for a claim whose agent has neither
   * begun a turn nor made a call the daemon saw. A caller in a pod whose plugin declares a
   * contract below 57 is served the state with no such key at all (the daemon's
   * `withoutStallRecord`), so every reader takes it as optional. */
  step: legionStepView.optional(),
  /** The wake the daemon's pacing holds for this claim, absent where it holds none: the same
   * wake `wakesWaiting` names beside this claim (contract 193). The kinds are the daemon's three
   * paced lanes, as `legionWakeWaiting` below names them. A caller below 193 is served no such
   * key (`withoutClaimRoundReport`). */
  wake: z.object(legionPacedWake).optional(),
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
  id: nonEmptyString.optional(),
  version: z.number().int().positive().optional(),
});

/** `api.PhaseView` — one phase worker's claim, the handoff of its current phase (the commit its
 * completion reported, the Dispatch handoff record it named), and the three counts the phase
 * carries: `pass`, the passes it has made through its stretch of the workflow, which every
 * backward move counts one of, `rounds`, the review rounds, which only a review that requested
 * changes counts, and `backwardMoves`, the counts of the moves back that made those passes. */
const legionPhaseView = z.object({
  claim: legionClaimView.optional(),
  handoffCommit: nonEmptyString.optional(),
  handoffRecord: legionHandoffRecordView.optional(),
  pass: z.number().int().nonnegative().optional(),
  rounds: z.number().int().nonnegative().optional(),
  /** How many times the issue's work has gone back by each kind of move, keyed `<from>-<to>` as
   * the daemon counts the pairs (`reviewing-implementing` and the rest). Absent, never `{}`, for
   * a worker whose issue has had no move back, and absent from the whole state for a caller in a
   * pod whose plugin declares a contract below 62 (the daemon's `withoutBackwardMoves`), so every
   * reader takes it as optional. `rounds` beside it counts every round, a conflict round
   * included, which no pair counts. */
  backwardMoves: z.record(z.string(), z.number().int().positive()).optional(),
});

/** `record.MergeState` — GitHub's own merge state for a pull request's head, in GitHub's
 * lowercase spelling, the eight words GitHub answers in `mergeable_state`: the daemon records
 * what it read and spells none of its own. The unread state is no word at all, so every field
 * that carries one is absent rather than empty while the record holds no state. */
const legionMergeState = z.enum([
  "clean",
  "unstable",
  "has_hooks",
  "blocked",
  "dirty",
  "behind",
  "draft",
  "unknown",
]);

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
  repo: nonEmptyString.optional(),
  number: z.number().int().positive().optional(),
  head: nonEmptyString.optional(),
  /** `open`, `merged` or `closed` (contract 36), `record.PullRequestState`'s values. */
  state: z.enum(["open", "merged", "closed"]).optional(),
  /** GitHub's own merge state for the head, in GitHub's lowercase spelling, as the daemon's last
   * read of the pull request found it (`record.MergeState`); absent until that read has run. It
   * says whether GitHub would merge the head now (`clean`) or holds it on a check or a review the
   * repository requires (`blocked`). */
  mergeState: legionMergeState.optional(),
  reviewDecision: nonEmptyString.optional(),
});

/** `api.MergeWaitView` — what an issue's review round waits on from GitHub (contract 133). It is
 * present only while the round waits on GitHub's merge state for the pull request's head
 * (`classify.RoundWaitsOnMergeState`): a reviewer's approval stands on the head the pull request
 * is at, and the merge state the record holds for that head is not one the issue may advance on.
 * It stands for the round waiting for a first answer and for the stuck round alike — both wait on
 * GitHub's next answer about the same head — and `toldHead` with `toldState` say what the architect
 * and the reviewer were last told of for it. The merge state alone rides on `pullRequest`, but
 * neither when it was read nor what the round's own wait is, which is what this carries. */
const legionMergeWaitView = z.object({
  /** The head the round waits on, the pull request's current head: the head the approval stands
   * on, and the head GitHub's answer is about. The daemon always writes it; the reader takes it as
   * optional, since no plugin code reads it (LEGION_ANSWER_READS). */
  waitsOn: nonEmptyString.optional(),
  /** The merge state the record holds for that head (`record.PullRequest.MergeState`), the same
   * word `pullRequest.mergeState` carries. Absent while the record holds no state for this head:
   * beside no `stateReadAt` the record holds no read of this head, and beside a `stateReadAt` it
   * holds a read whose answer carried no state. */
  mergeState: legionMergeState.optional(),
  /** `record.PullRequest.StateReadAt`, the moment GitHub's last answer about this head was in
   * hand, a confirming answer included. Absent when the record holds none; absent beside a
   * recorded `mergeState`, the record holds a state with no read time. */
  stateReadAt: timestamp.optional(),
  /** The newest `updated_at` GitHub gave among the pull request's lifecycle observations: a
   * reopen at the same head moves it, and a push whose event carried no clock does not raise it.
   * Absent while the record holds none. */
  headUpdatedAt: timestamp.optional(),
  /** How many of the fence's check runs are stamped with the current head
   * (`record.PullRequest.CheckRuns`): the runs a CI settlement has applied for this head. The
   * listener settles a head only once every check it has seen there is terminal, so 0 is no
   * evidence of what GitHub holds: CI still running, no check at all, and runs that concluded with
   * their settlement not yet arrived all read 0, so read the head's runs and their status on
   * GitHub before acting. Written whatever its value, so 0 is that reading and not a gap. The
   * reader takes it as optional, since no plugin code reads it (LEGION_ANSWER_READS). */
  checkRuns: z.number().int().nonnegative().optional(),
  /** The head and the merge state the architect and the reviewer were told of
   * (`record.ReviewDecision.ToldStuckHead` and `ToldStuckState`, the marks workflow's
   * `tellStuckMergeState` leaves). They name what the reviewer's decision last records as told,
   * whatever the merge state and the read time say now, and a later answer may have moved past
   * them; absent when that decision records none. */
  toldHead: nonEmptyString.optional(),
  toldState: legionMergeState.optional(),
});

/** `api.GateView` — the design gate; `approvedVersion` is null until a human approves one, and
 * the gate is open exactly when it equals `currentVersion`. A spec step that stands open on an
 * ancestor's approval (entry 20261125161152 (gate-basis)) holds the version its root registered there, which no person
 * approved: the step's entry in `gatedSteps` reads `open_on_basis` and names that approval, so a
 * reader that names a person's approval reads `gatedSteps`, not this. */
const legionGateView = z.object({
  artifactId: nonEmptyString.optional(),
  currentVersion: z.number().int().positive().optional(),
  approvedVersion: z.number().int().positive().nullable().optional(),
});

/** `record.DesignStep` — one step of a deployment's design gate, each naming one document of the
 * root issue: `decisions` its decisions document, `criteria` its Verification document, `spec`
 * its primary document. */
const legionDesignStep = z.enum(GATED_STEPS);

/** `api.GatedStepBasis` — the ancestor's approval a step stands open on, or the ancestor's open
 * approval request it waits on (entry 20261125161152 (gate-basis)): the ancestor `issue`, its document's `artifactId`
 * and the `version` a person approved or is asked to approve; for an approval, `approver`, who
 * gave it as Dispatch names an actor (`user:<login>`), and `approvedAt`, Dispatch's time of it
 * where Dispatch gave one; for a request, its `askId`. */
const legionGatedStepBasis = z.object({
  issue: issueKey,
  artifactId: nonEmptyString.optional(),
  version: z.number().int().positive(),
  approver: nonEmptyString.optional(),
  approvedAt: timestamp.optional(),
  askId: nonEmptyString.optional(),
});

/** `api.GatedStepView` — one step of a tree's design gate (`record.TreeSteps`) as its root
 * stands in it (contract 55). `artifactId` and `currentVersion` are the document its architect
 * registered for the step and that document's current version, absent until one is registered;
 * `approvedVersion` is the version a person approved, null until one does. `status` is where the
 * step stands: `unregistered` (the deployment gates it and the issue holds no record of it, so it
 * holds the tree's work), `waiting` (registered, and no person has reviewed its current version),
 * `approved` (a person approved its current version, which lets the work past the step) or
 * `changes_requested` (its reviewer asked for changes at its current version, with
 * `changesReason` the reason they gave; absent under every other status). Entry 20261125161152 (gate-basis) adds two
 * words and `basis` beside them: `open_on_basis`, a step its architect registered on a person's
 * approval of an ancestor's document, which lets the work past the step as an approval does and is
 * no person's approval of the root, its `approvedVersion` being the root's version registered; and
 * `pending_on_basis`, a step that waits, closed, on the ancestor's open approval request, which a
 * person's approval of that version opens, its `approvedVersion` being a person's approval of the
 * root's own earlier version, if any. `basis` is present under those two words alone. A daemon at
 * that contract serves a caller below it no `gatedSteps` on an issue any step of which reads either
 * word. */
const legionGatedStepView = z.object({
  step: legionDesignStep.optional(),
  artifactId: nonEmptyString.optional(),
  currentVersion: z.number().int().positive().optional(),
  approvedVersion: z.number().int().positive().nullable().optional(),
  status: z
    .enum([
      "unregistered",
      "waiting",
      "approved",
      "changes_requested",
      "open_on_basis",
      "pending_on_basis",
    ])
    .optional(),
  changesReason: nonEmptyString.optional(),
  basis: legionGatedStepBasis.optional(),
});

/** `api.SlotView` — the admission slot the issue occupies and when it took it. */
const legionSlotView = z.object({
  index: z.number().int().nonnegative().optional(),
  admittedAt: timestamp.optional(),
  lentTo: issueKey.optional(),
});

/** `api.WaitingLineView` — where the issue stands in `admission.waiting`: `place`, 1 being first
 * in the line, and `length`, how long that list was at this read, beside the line's own fields
 * for a program that acts on the line rather than reading the sentence `reason` renders:
 * `priority`, the Dispatch priority band the issue waits in: the priority Dispatch resolves for
 * it (or, with an older Dispatch release that sends no `resolved_priority`, its own priority else
 * its tree root's for line order alone), 0 for P0 to 3 for P3, or null for an issue placed after
 * P3; `part`, which of the two parts of its band the issue waits in, and
 * `kind`, why it waits there; `partPlace` and `partLength`, its place within its own band and
 * part and that part's length in that band; `holdsSlot`, whether it already holds one of the
 * cap's slots; and what placed it within its part — `since`, the moment its work came back, for
 * a member of the `returned` part, and `rootRank`, `ownRank` and `rootKey`, the Dispatch ranks
 * and the tree root's key, for a member of the `starting` part. The line is ordered by band
 * first, P0 to P3 and then the band of no priority, and by part and its key within one band. It
 * says where the issue stands, not what holds it, which `admission.blocked` names.
 *
 * Every member below `length` is optional, for two readers at once: a member carries the fields of
 * its own part alone, so a returned member names no rank and a starting member no `since`; and a
 * caller below the contract that brought `partPlace` and the rest of them, contract 85, is served
 * `place` and `length` alone, with no field of theirs on the wire at all; `priority` came with
 * contract 250, and a caller below it is served every member without it.
 *
 * `part` and `kind` are written as the enums they are, which is how `LEGION_WAIT_ANSWERS` writes
 * the acts that end a root's wait: a `z.enum` refuses a value it does not name rather than dropping
 * it, so a word a later daemon adds would refuse the whole state body here — and a daemon that adds
 * one declares a new contract for it, as the ninth answer did at contract 68, which is what keeps
 * this reader away from a body that carries it. Nothing older reaches them either: both words are
 * rendered afresh from `record.LinePart` and `record.LineKind` at each read and never stored, so no
 * retired word comes back off a row, and naming the five kinds is what lets a reader branch on
 * them. `waiting_on_issue`, a started issue that gave its admission slot back to wait on another
 * issue, is the fifth, added at contract 132; a caller below 132 is served such a member without
 * its `kind`. */
const legionWaitingLineView = z.object({
  place: z.number().int().positive().optional(),
  length: z.number().int().positive().optional(),
  part: z.enum(["returned", "starting"]).optional(),
  kind: z
    .enum(["returning", "answered", "queued_root", "first_phase", "waiting_on_issue"])
    .optional(),
  partPlace: z.number().int().positive().optional(),
  partLength: z.number().int().positive().optional(),
  holdsSlot: z.boolean().optional(),
  since: timestamp.optional(),
  rootRank: nonEmptyString.optional(),
  ownRank: nonEmptyString.optional(),
  rootKey: issueKey.optional(),
  priority: dispatchPriority,
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
  state: z.enum(["claim_stopped", "waiting", "blocked", "unlisted"]).optional(),
  reason: nonEmptyString.optional(),
});

/** `api.CheckWakeView` — what woke a production check's latest look: `kind` names the cause
 * (`merge` for the look the close opens, `check wake` for one the checking agent's own `wake_at`
 * or the retry of a look nobody reported starts, and `bound` for the last look; a check whose
 * look was woken before the daemon stopped reading production can hold one of the causes that
 * are gone), `detail` the cause's own detail — for `check wake`, the summary the agent's report
 * gave or the retry's own words — `at` when. */
const legionCheckWakeView = z.object({
  kind: nonEmptyString.optional(),
  detail: z.string().optional(),
  at: timestamp.optional(),
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
 * settings. These members are optional: the tool reads no check state to choose a call or
 * validate a look. The daemon validates the named check; read_record passes this record on.
 */
const legionCheckView = z.object({
  repo: nonEmptyString.optional(),
  pullRequest: z.number().int().positive().optional(),
  mergeCommit: z.string().optional(),
  openedAt: timestamp.optional(),
  role: nonEmptyString.optional(),
  looks: z.number().int().nonnegative().optional(),
  lookOpen: z.boolean().optional(),
  lastWake: legionCheckWakeView.optional(),
  nextWakeAt: timestamp.optional(),
  last: z.boolean().optional(),
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
 * waiting line, within its priority, among the issues whose work came back, oldest first, until
 * the pass that resumes it. */
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
    z.object({ kind: z.literal("ask"), ask: nonEmptyString.optional(), ...legionWaitTimes }),
    z.object({
      kind: z.literal("gate"),
      gateVersion: z.number().int().positive().optional(),
      artifactId: nonEmptyString.optional(),
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
  issue: nonEmptyString.optional(),
  payload: z.record(z.string(), z.unknown()).optional(),
  attempts: z.number().int().nonnegative().optional(),
  nextAt: timestamp.optional(),
  lastError: z.string().optional(),
});

/** `api.StrandedClaim` — one claim nothing will wake (`workflow.Stranded`): a worker of its issue's
 * phase, or its tree's architect, in a tree that runs, named by its issue, its role and its state;
 * the issue's entry under `issues` carries the rest. Its state, role and suspension say the move
 * that brings it back (`api.StrandedClaim.Move`). A `retired` entry is a claim a close or a stop
 * retired while its tree runs: only a start of the phase it owns relaunches it, and no rule the
 * daemon runs queues that start, so nothing will wake it either. That state arrived with contract
 * 56, and a pod whose claims record a contract below it is served this report without its retired
 * entries (`api`'s `stateFor`, `withoutUnadmittedStrandedStates`) rather than being made to
 * refuse the whole state over a third value its enum does not name.
 *
 * Who suspended a suspended one is not here and must never be added: a session parses this body
 * with the schema of the contract its plugin was built at, and a Kubernetes deployment relaunches
 * no stale session, so a key added here refuses the whole state in every pod still on an image
 * whose plugin predates INFRA-1048, where the answer side still refused a key it did not name.
 * The operator claim routes serve it, and no plugin calls them
 * ("no session-facing response carries suspendedBy" in legion-api.test.ts). */
const legionStrandedClaim = z.object({
  issue: issueKey.optional(),
  role: z.enum(LEGION_ROLES).optional(),
  state: z.enum(["suspended", "failed", "retired"]).optional(),
});

/** `api.HeldOutboxRow` — one due lifecycle outbox row its issue's lifecycle lane cannot run yet,
 * because another lifecycle row of the issue is running (`record.Store.HeldRows`, contract 28):
 * the lane runs an issue's lifecycle rows one at a time, in the order they were queued. `waitedSeconds`
 * counts from the moment the row became due, so a retried row's earlier backoffs are not counted as
 * waiting on its sibling; `waitsOn` is the id of the running row — or, for a role's start fenced
 * behind its issue's branch row, that branch row while it runs. */
const legionHeldOutboxRow = z.object({
  id: z.number().int().positive().optional(),
  kind: nonEmptyString.optional(),
  issue: nonEmptyString.optional(),
  waitedSeconds: z.number().int().nonnegative().optional(),
  waitsOn: z.number().int().positive().optional(),
});

/** `api.WakeWaiting` — one wake the daemon's pacing is holding until its turn
 * (`worker_wakes_per_minute`, `worker_wake_burst`, contract 53): the claim it will wake, that
 * claim's issue and role, what kind of wake it is, and `dueAt`, the pacing's estimate at the read
 * of the moment it lets the wake out, as its queues stand (entry 20261125211005). A waiting wake
 * holds a place and no moment of its own. A change to the queues or to the pace can move that
 * moment earlier or later, so two reads can name two moments: for example, a wake ahead of it in
 * its own queue that leaves moves it earlier, and a wake that joins and is let out ahead of it
 * moves it later. The kinds are the daemon's three paced lanes: `launch`, a role process it is
 * about to start, and `plugin-refresh`, the relaunch of a process whose plugin speaks an earlier
 * contract than the daemon's, which the stale-plugin lane's own gate admits (contract 73), both
 * held in the claim's machine and lost with a restart, which raises the wave again through its own
 * paths; and
 * `catch-up`, the notice a start writes to an architect already running in place of a launch,
 * whose outbox row stands held while its place waits in the queue. `kind` is an unconstrained
 * string, so a lane a later daemon names is read rather than refused. `dueAt` is required: the
 * controller's `read_state` summary names the earliest of them (`pi-legion/src/tools.ts`), and
 * the daemon writes it on every wake. The rest is optional, since no plugin code reads it
 * (LEGION_ANSWER_READS). */
const legionWakeWaiting = z.object({
  claim: nonEmptyString.optional(),
  issue: issueKey.optional(),
  role: z.enum(LEGION_ROLES).optional(),
  ...legionPacedWake,
  dueAt: timestamp,
});

/** `api.AssignmentView` — one experiment's answer for an issue (contract 141): the name the
 * project declares the experiment under, the arm the draw gave the unit, the mode in force at
 * that draw and the instant of it. `variant` and `mode` are strings and not enums: the arms are
 * the operator's own names, the empty string is a unit settled at no arm, and a mode a later
 * daemon adds must be read rather than refuse the whole state. The daemon always writes
 * `experiment`, `variant` and `mode`; the reader takes them as optional, since no plugin code reads
 * them (LEGION_ANSWER_READS). `plan` (contract 220) is the group the arm set at the draw, `owner`
 * or `default`, which the issue runs on for its whole life whatever the settings say later; it is
 * absent where the arm named none, and a string rather than an enum, so a group a later daemon
 * carries out is read rather than refuse the state. */
const legionAssignment = z.object({
  experiment: nonEmptyString.optional(),
  variant: z.string().optional(),
  mode: z.string().optional(),
  drawnAt: timestamp.optional(),
  plan: nonEmptyString.optional(),
});

/** `api.PodHoldView` — the standing hold of an issue's pod (contract 246): why the daemon's idle
 * rule keeps that pod running past the idle period. `reason` is the rule's own word for the hold
 * (`workflow.IdleHold`'s name, as the daemon's hold records spell it: `background_work`, `image`,
 * `delivery_waiting` and the rest), a string and not an enum, so a hold a later daemon adds is
 * read rather than refused. `firstAt` is the first pass of the hold's streak and `at` its newest
 * pass, so a reader tells a streak the passes still read from one whose passes stopped. `detail`
 * is what the reason leaves out, such as the kinds a background-work hold's session reported, cut
 * to its first 200 characters and absent where the record holds none. The daemon always writes
 * `reason`, `firstAt` and `at`; the reader takes every member as optional, since no plugin code
 * reads them (LEGION_ANSWER_READS). */
const legionPodHold = z.object({
  reason: nonEmptyString.optional(),
  firstAt: timestamp.optional(),
  at: timestamp.optional(),
  detail: nonEmptyString.optional(),
});

/** `api.Issue` — `workers` is keyed by a plain string, not by the role names this build knows, so
 * a role a later daemon adds is read rather than refused; the key list is held by the golden test
 * in `legion-api.test.ts`, not by this schema. A phase that has not run has no entry, so the value
 * is optional. `waitingOn` is present only in `awaiting_person`; `check` is the issue's open
 * production check, present only on a `done` issue while the check is open. */
const legionIssue = z.object({
  key: nonEmptyString.optional(),
  generation: z.number().int().nonnegative().optional(),
  phase: z.enum(LEGION_PHASES),
  status: nonEmptyString.optional(),
  /** The key of the root of the issue's tree, equal to `key` on a root (contract 51). A child
   * holds an admission slot of its own, so neither `admission.active` nor `admission.waiting`
   * says which tree an issue belongs to; a reader tells a child from a root here, and finds the
   * members of a tree that lingers. Absent on an issue the workflow does not record (phase
   * `unrecorded`, an operator's claim), which stands in no tree, and absent from every issue of
   * the body the route serves a caller before 51 (`api.stateFor`, `withoutTrees`). */
  tree: issueKey.optional(),
  /** Whether the tree this root roots runs under the workflow, as the daemon's own rule answers it
   * (`record.TreeRuns`, entry 20261125161152 (gate-basis)): its lifecycle stands open under the workflow, the root does
   * not linger, and the root does not wait in the line for that tree to be opened. On every root
   * the daemon records and on no other issue, since no other field decides it in every case: a
   * root a re-admission left at `todo` waiting for a slot runs nothing whatever its architect's
   * claim reads, and a root a person answered runs while it waits in the line for its resume.
   * Absent from every issue of the body the route serves a caller before entry 20261125161152 (gate-basis)
   * (`api.stateFor`, `withoutGateBasis`), which is why a reader takes it as optional. */
  treeRuns: z.boolean().optional(),
  /** The repository (`owner/name`) the issue targets and that repository's default branch, taken
   * when the daemon first recorded the issue and never changed after; absent on an issue the
   * workflow does not record (phase `unrecorded`, an operator's claim) (contract 31). */
  repo: nonEmptyString.optional(),
  defaultBranch: nonEmptyString.optional(),
  /** Why a held issue is held, when its hold has one: `escalated` once its architect sent it to
   * the controller; and from contract 219 `task-stopped` once its owner stopped its task with
   * stop_task, and `children` while a parent whose task's last step is not the owner's acceptance
   * waits, before its retro, for the issues beneath it, the roots that rest on its approval or its
   * open Dispatch children. A caller whose contract (the one it states, else its pod's
   * lowest) is below 219 is served an issue held for either of those two with no `holdReason`
   * (the daemon's value strip `withoutUnadmittedHoldReason`). `heldFrom` beside it is the phase
   * the hold took the issue out of, and is absent for an issue that is not held. The linger
   * suppression is `holdReason`'s alone:
   * the reason is absent while the issue's tree lingers or is closed, until it is re-admitted,
   * since it is a decision someone must act on and a lingering tree has none; the phase is a fact
   * of the record that costs one word, so every issue at `phase: held` names the phase that
   * stopped, whatever its tree is doing. A caller in a pod whose plugin declares a contract
   * below 75 is served every issue without `heldFrom` (the daemon's `withoutRecordFields`), so
   * every reader takes it as optional. */
  holdReason: z.enum(["escalated", "task-stopped", "children"]).optional(),
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
  /** The issue's own Dispatch priority, 0 for P0 to 3 for P3, or null for an issue with none
   * (contract 250): the priority Dispatch sends on the issue itself. Admission reads
   * `resolvedPriority` where Dispatch sends one; the line's own `priority` says which band placed
   * a waiting issue. Present on every issue the workflow records; absent on an issue it does not
   * record (phase `unrecorded`, an operator's claim), and from every issue of the body the route
   * serves a caller below 250. */
  priority: dispatchPriority,
  /** The priority Dispatch resolves the issue to, 0 for P0 to 3 for P3, or null for none (the P0
   * start's contract entry, dispatch://INFRA-5636): its own priority, else the nearest issue's above
   * it on its Dispatch parent chain that has one. It is the priority admission reads, and `priority`
   * keeps its meaning. Absent where Dispatch sent no resolved priority, as a Dispatch release before
   * dispatch://INFRA-5793 sends none, and from every issue of the body the route serves a caller
   * below the entry. */
  resolvedPriority: dispatchPriority,
  /** The key of the issue `resolvedPriority` comes from (Dispatch's `priority_from`), absent where it
   * is the issue's own or none. */
  priorityFrom: issueKey.optional(),
  workers: z.partialRecord(z.string(), legionPhaseView).optional(),
  pullRequest: legionPullRequestView.optional(),
  designGate: legionGateView.optional(),
  /** The design gate as this issue stands in it, one entry for each step that holds its tree: the
   * steps the deployment gates, in the order it gates them (contract 55), then `criteria` where
   * the deployment's list names `spec` and not `criteria` and the root registered its
   * Verification document. This daemon always writes it, and it is empty for an issue that is not
   * its tree's root, for a deployment that gates no step and for an issue the workflow does not
   * record; a daemon before contract 55 writes none, which is why a reader takes it as absent
   * rather than as an empty list.
   * `designGate` above is unchanged beside it, the `spec` step's row, which a reader that asks one
   * question of one gate takes. A caller from before the two basis words of `legionGatedStepView`
   * is served no `gatedSteps` on an issue any step of which reads one of them. */
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
   * is served this key too, and one below contract 85 is served neither (`api.shapeIssue`,
   * `withoutWhyAndLineFields`). */
  why: legionWhyView.optional(),
  waitingOn: legionWaitingOn.optional(),
  check: legionCheckView.optional(),
  /** `mergeWait` is what this issue's review round waits on from GitHub (contract 133): present
   * only while the round waits on GitHub's merge state for the pull request's head, so every
   * issue whose round waits on nothing from GitHub has none, and a daemon before `mergeWait`'s
   * contract 133 writes none at all (`api.shapeIssue`, `withoutMergeWait`), which is why a reader
   * takes it as optional. */
  mergeWait: legionMergeWaitView.optional(),
  /** The memory group of the issue's tree: `memory` for a tree whose roles run with the project's
   * memory overlays and instructions, `none` for one that runs without them, with the mode in
   * force when the group was drawn and the instant of that draw. The three are the answer of the
   * experiment named `memory` in `assignments` below, read only where that row records a draw
   * and spelled out as this key has read it since contract 49: the arm `memory` is the group
   * `memory` and every other arm — the control `none` and the empty arm an experiment at mode
   * off draws — is `none`. They are absent together while no `memory` answer applies to the
   * issue — a tree first opened before the setting existed — and while its only `memory` answer
   * records no draw, which is a hand-over settling the tree at no arm and which answered none
   * of the three before contract 141 as well. Every issue of a tree carries its tree's, since
   * the draw is made once, for the unit (contract 49). */
  memoryGroup: z.enum(["memory", "none"]).optional(),
  memoryDrawMode: z.enum(["off", "on", "random"]).optional(),
  memoryDrawnAt: timestamp.optional(),
  /** The entry the role of the issue's current phase stands on, and the entry that role was last
   * given a task of (contract 94). The daemon sends the entry's own work to a role whose given
   * number is below the entry its row names, on every admission pass and on a sweep of its own
   * each minute, so the two are equal for a role holding its work and differ for one the daemon
   * has still to reach. A root carries them as a child does, since the architect is the role
   * that works `admitted` (contract 146); both are absent together for a phase no role works —
   * `awaiting_person`, `awaiting_children`, `done` and `held` — and each is absent where it is 0,
   * which is a role no entry has been raised for and one the daemon holds no claim row of; a
   * daemon before contract 94 writes neither, and a caller in a pod whose plugin declares a
   * contract below 94 is served the state with neither (the daemon's `withoutPhaseEntries`),
   * which is why a reader takes both as optional. */
  phaseEntry: z.number().int().nonnegative().optional(),
  givenEntry: z.number().int().nonnegative().optional(),
  /** The two times of a claim's handshake of the last entry it was queued a task of (contract
   * 146): when a task of that entry was first queued to the claim, and when a turn of that
   * claim's worker first confirmed one. The claim keeps both in a record of its own, so they
   * outlast the task, which the end of the confirming turn retires. A tree's own root carries its
   * architect's handshake whatever phase it stands in — through `awaiting_person` and the phases
   * a phase role works, where `phaseEntry` and `givenEntry` are that role's — and any other issue
   * the handshake of the role of its current phase. Both are absent where no entry was queued to
   * that claim in the issue's current generation — a role the daemon holds no claim row of, a
   * re-admitted root whose new architect entry is not queued yet, and a root of the release
   * before contract 146, which carries the two numbers and no handshake — and `entryConfirmedAt`
   * alone is absent while no turn has confirmed a task of the entry. A daemon before contract
   * 146 writes neither, and a caller in a pod whose plugin declares a contract below 146 is
   * served the state with neither (the daemon's `withoutEntryTimes`), which is why a reader takes
   * both as optional. */
  entryQueuedAt: timestamp.optional(),
  entryConfirmedAt: timestamp.optional(),
  /** For a tree's own root, every arm of the daemon's architect-stalled reading that holds for its
   * architect at this read, in the order the controller is told them (contract 146):
   * `took-nothing`, the task of the entry its `phases` row names was first queued to its claim more
   * than ten minutes ago and no turn confirmed it, counted from that first queueing however many
   * times the daemon sent that entry's task again, where a task whose turn ended with the claim's
   * process (a death, or a replacement the daemon made) counts only while the claim is ready,
   * working or idle, and from the relaunched agent's ready where that is later, and where a task
   * the root's own record has superseded — one that carries an entry, of a run before its
   * generation or of an entry the row has moved past — raises the arm not at all, nor does a task
   * of no entry, such as a wake's; `claim-unreachable`, the claim is failed or
   * retired and no start of the root's run is the daemon's next move for it (a start the outbox is
   * running is, and so is a start queued in the last ten minutes unless it has already run against
   * the claim and left it failed or retired, after which the outbox only runs it again on its
   * backoff), or no architect claim stands, no start of one was queued in the last ten minutes,
   * and the work of its entry has waited the same ten minutes, which holds of a root waiting on a
   * person as of any root, since the daemon queues the next start of such a root's architect only
   * when that wait is answered; `slow-before-gate`, it stands at `admitted` with no design-gate
   * step registered 90 minutes after the later of its admission and its architect's last raised
   * entry. The daemon reads the arms by the predicates its own `architect-stalled` wake uses and
   * serves them whether or not the controller was told, and `legion state`'s plain line prints
   * them and names a root for nothing else. Absent for a root no arm holds for and for every issue
   * that is not a tree's root; a daemon before contract 146 writes none, and a caller in a pod
   * whose plugin declares a contract below 146 is served the state without it (the daemon's
   * `withoutEntryTimes`), which is why a reader takes it as optional. */
  architectStalls: z
    .array(z.enum(["took-nothing", "claim-unreachable", "slow-before-gate"]))
    .optional(),
  /** When the daemon last saw anything of this issue's tree move, and how long that tree has
   * been still at this read, in hours (contract 111). The pair is the tree's and not the issue's,
   * so every issue of one tree carries the same two values — an issue in `accepting`, `retro` or
   * `merging`, which holds no admission slot, and a done child recorded under a live tree among them. Both are
   * absent together for an issue whose tree is not live and for an issue the workflow does not
   * record, and a daemon before contract 111 writes neither at all, which is why a reader takes
   * them as optional. A tree the record shows no movement for is measured from the zero time,
   * where the daemon's duration saturates, so `quietHours` reads 2562047.788 — 292 years, the
   * largest span it holds. That is "this tree has never moved", not a fault to refuse the body
   * over, which is why the bound here is `nonnegative` and no ceiling. */
  treeMovedAt: timestamp.optional(),
  quietHours: z.number().nonnegative().optional(),
  /** Every experiment answer that applies to the issue (contract 141): its tree's, for an
   * experiment whose unit is the tree, and its own, for one whose unit is the issue, in the
   * order the project's file declares the experiments. That is the `memory` block's experiment
   * first (`settings.projects.<KEY>.memory`, which every project declares, at mode `off` where its
   * file writes no block), unless `settings.projects.<KEY>.experiments` names `memory` itself;
   * then the experiments `settings.projects.<KEY>.experiments` names, in its order; then any
   * experiment the file no longer names, in the order of their names. `variant` and `mode` ride
   * whatever they hold, the empty string included, and `drawnAt` is absent where the row holds
   * no instant: that row — no arm, no mode, no instant — is a hand-over settling a unit at no arm
   * for an experiment the tree it left held no row for, which is how a reader tells a unit
   * settled at no arm from a unit never drawn, which carries no row at all. The key is absent
   * from an issue no experiment holds an answer for, and from every issue of the body the route
   * serves a caller before 141, so a reader takes it as optional. */
  assignments: z.array(legionAssignment).optional(),
  /** The standing hold of the issue's pod (contract 246, `legionPodHold`): present only while a
   * hold of the idle rule stands on that pod, and absent for a pod the rule has not held, one it
   * released and one whose streak a later pass ended. A daemon before contract 246 writes none,
   * and a caller whose plugin declares a contract below 246 is served every issue without it
   * (the daemon's `withoutPodHold`), which is why a reader takes it as optional. */
  podHold: legionPodHold.optional(),
});

/** `api.DaemonInfo` — the running daemon: its project, its store's schema, and its boot history.
 * No plugin code reads a field of it: the controller's boot check read `project` here until it
 * read the health answer instead, and the controller's `read_state` hands the state to its
 * agent whole. So every field is optional, as `LEGION_ANSWER_READS` requires of a field no read
 * in plugin code names. */
const daemonInfo = z.object({
  project: nonEmptyString.optional(),
  schemaVersion: z.number().int().nonnegative().optional(),
  boots: z.number().int().nonnegative().optional(),
  firstBootAt: timestamp.optional(),
  startedAt: timestamp.optional(),
});

/** `api.ContractView` — the mark of the narrowing, beside the daemon's own block: `daemon` is the
 * contract the daemon declares and served this body at (its `DaemonAPIVersion`), and `caller` the
 * contract the daemon shaped this body to (contract 75). From contract 193 on the daemon writes
 * `caller` for every reader. A reader served the body whole — the operator's
 * `legion state --json`, a tmux pane, a pod whose claims record no contract, a reader at the
 * daemon's own contract — is told the daemon's own number. Before contract 193 a daemon from 75 on
 * left `caller` off for such a reader, which is why the key stays optional.
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
  daemon: z.number().int().positive().optional(),
  caller: z.number().int().positive().optional(),
});

/** `api.BlockedIssue` (`record.BlockedIssue`, and `api.HeldChild` on a wave release) — one issue
 * Legion waits on another issue for, one it has still to start or one it started that gave its
 * admission slot back for the wait, and the issues it waits on that are not done, in the order
 * Dispatch lists them. A blocker the daemon has not yet observed as an issue of its own counts as
 * unfinished, so `blockedBy` can name a key the state's `issues` does not. */
const legionBlockedIssue = z.object({
  issue: issueKey.optional(),
  blockedBy: z.array(issueKey).optional(),
});

/**
 * `record.HeldReason` — the words of `admission.held`, in `record.HeldReasons`' order.
 * `children_before_retro` and `task_stopped` came with contract 219; a caller below it is served
 * each such row as `unclassified` with no `heldBy` (the daemon's value strip
 * `withoutUnadmittedHeldWords`). `awaiting_children` came with entry 20261125161152 (gate-basis); a caller below it is
 * served each such row as `children_first`, the one word that strip stands another in for
 * (`heldReasonStandIn`).
 * `at_work` names an issue in `accepting`, `retro` or `merging`, which keeps no admission slot;
 * `record.HeldAtWork` says which issues read it.
 */
export const LEGION_HELD_REASONS = [
  "tree_closed",
  "tree_parked",
  "label_off",
  "ancestry",
  "root_awaiting_person",
  "awaiting_children",
  "design_gate",
  "dispatch_blocker",
  "children_first",
  "accepted_ancestor",
  "children_before_retro",
  "task_stopped",
  "at_work",
  "unclassified",
] as const;

/** `api.HeldIssue` (`record.HeldIssue`) — one row of `admission.held`: an issue the daemon's
 * record holds in the workflow that stands in neither `active` nor `waiting`. The daemon runs
 * nothing of it and queues it nowhere, and the row says in one word why (`reason`, the closed
 * list one ladder reads in one order) and which issues hold it (`heldBy`). A row names no issue
 * of `active` or `waiting`: the three lists are one partition of the issues the record holds in
 * the workflow, so a reader with all three has every issue and what each is doing. `heldBy` is
 * absent, never empty, where no issue holds the one that waits — a root whose Legion label is
 * off, or an issue none of the other words is true of — and a key it names may be one the state's
 * `issues` does not hold, as `blockedBy` may today, since an issue the daemon has not observed
 * holds work all the same. */
const legionHeldIssue = z.object({
  issue: issueKey.optional(),
  reason: z.enum(LEGION_HELD_REASONS).optional(),
  heldBy: z.array(issueKey).optional(),
});

/** `api.PauseActorView` — who paused the project or ended its pause (contract 178), and who set the
 * P0 bound while the daemon runs (the P0 start's contract entry): the controller, with the session it acted from,
 * or the operator, who acts from none. The daemon's store holds these two shapes and no other
 * (`project_pauses_by_session` and `project_pauses_ended_by_session` in its migration
 * `20261115194357_project_pause`, `project_p0_bound_changes_by_session` in
 * `20261125181828_p0_bound`), so a controller's entry always names its session, and the reader
 * requires it: the controller's `read_state` summary names the session of each controller it prints
 * (LEGION_ANSWER_READS). */
const legionPauseActor = z.discriminatedUnion("kind", [
  z.object({ kind: z.literal("controller"), session: nonEmptyString }),
  z.object({ kind: z.literal("operator") }),
]);

/** A P0 bound: how many P0 issues may hold an admission slot at once, a whole number from 1 to
 * 2147483647, the most the daemon's store holds: the daemon refuses a set of any other number
 * (`INVALID_P0_BOUND`) and a legion.yaml whose `admission_p0_bound` is any other. The plugin's tool
 * checks a bound with it before it sends one, so a change of the range reaches both. */
export const LegionP0BoundNumber = z.number().int().positive().max(2147483647);

/** `api.P0BoundView` — the P0 bound (the P0 start's contract entry, dispatch://INFRA-5636): how many P0 issues may
 * hold an admission slot at once, outside the cap. `bound` is the bound in force, null for none;
 * `configured` is legion.yaml's `admission_p0_bound`, null where the file gives none; and `live` is
 * the value the controller or the operator set while the daemon runs and that stands — its `bound`,
 * null for a set of none, who set it, when and why — absent where none was set or the newest change
 * cleared it. The state's `admission.p0Bound` and the answer to the controller's set and clear of
 * the bound are this view. The controller's `read_state` summary prints the bound in force and the
 * live value (LEGION_ANSWER_READS), so those are required; `configured` is read by no plugin code. */
const legionP0Bound = z.object({
  bound: LegionP0BoundNumber.nullable(),
  configured: LegionP0BoundNumber.nullable().optional(),
  live: z
    .object({
      bound: LegionP0BoundNumber.nullable(),
      setBy: legionPauseActor,
      setAt: timestamp,
      reason: nonEmptyString,
    })
    .optional(),
});

/** `api.Admission` — the issue cap and the issues under it, in Dispatch rank order. Concurrency
 * is capped on issues, never on workers: there is no worker queue on this wire. Since contract 51
 * the cap counts the issues the daemon has started rather than the trees (dispatch://INFRA-980):
 * `active` is every issue holding a slot and `waiting` the admission line, children as much as
 * roots, so one tree with three started children holds three slots. An issue holds one of the
 * cap's slots while it stands in a phase that keeps one, from the moment the daemon starts it
 * until it is done or Dispatch takes it out of the workflow: `accepting`, `retro` and `merging`
 * hold none (in the default steps, the phases after the review), and a step after `accepting`
 * whose phase keeps a slot waits for one again. So `active` carries an issue at work, one `held`
 * and one waiting on a person, while an issue in `accepting`, `retro` or `merging` — a merger at
 * work in `merging`, one in `merging`
 * whose start a closed design gate holds, a merged root standing in `merging` while the rest
 * of its tree runs — and one whose phase has left the workflow, `done` or a status Dispatch
 * took it out of the workflow with, are in neither list. A started issue that waits on another
 * issue in a phase that keeps a slot is missing from `active` too, other than the three roots that keep
 * their slot through such a wait: it holds no slot while it waits and stands in `waiting` and in
 * `blocked`, which is how a reader tells it from an issue a person parked, which stands in none
 * of the three (the rule: "Admission and the `legion` label" in
 * docs/site/src/content/docs/legion/concepts.md). An issue holding a slot is in
 * `waiting` as well while its work comes back to it, an answered root until the pass that
 * resumes it. `trees` is the root of every live tree, sorted, which a reader needs because a
 * running tree may hold no slot at all — the controller reads it where it read `active` as the
 * running
 * trees. A daemon at this contract writes it on every state, empty rather than null, and it is
 * optional here only because that daemon leaves it off one body: a reader before 51 refuses the
 * key outright, so the route strips `trees` and the issues' `tree` for a caller in a pod whose
 * claims record a contract below 51 (`api.stateFor`, `withoutTrees`). A reader that finds it
 * absent has been served that stripped body and learns nothing about which trees run; it is
 * never a daemon older than this plugin, whom the boot gate and `legion probe-image` refuse.
 * `blocked` is every issue Legion waits on a Dispatch issue nobody has finished for, by issue
 * key: a root or a child Legion is waiting to start, and a started issue that gave its slot back
 * for the wait; a blocked issue the line carries is in `waiting` too.
 * The controller names them in the day's report and takes nothing for them.
 * `held` is every issue the daemon's record holds in the workflow that stands in neither
 * `active` nor `waiting`, sorted by key: the work the daemon runs nothing of and queues nowhere,
 * each row saying in one word why and naming the issues that hold it. A row names no issue of
 * `active` or `waiting`, so the three lists are one partition of the workflow and a reader of
 * all three can say of every issue what it is doing; `heldBy` is absent where no issue holds the
 * one that waits, and a key it names may be one the state's `issues` does not hold, as
 * `blockedBy` may. A daemon at this contract writes `held` on every state, empty rather than
 * null, and it is optional here for the one reason `trees` is and no other: a reader before the
 * contract that added it refuses the key outright, so the route strips it for a caller in a pod
 * whose claims record a contract below that one (`api.stateFor`, `withoutHeld`). A reader that
 * finds it absent has been served that stripped body and learns nothing about what is held.
 * `p0Holders` is how many of the issues holding a slot admission treats as P0 (Dispatch's
 * resolved P0, or own P0 with an older Dispatch release), which start and hold their slot outside
 * the cap, so the cap counts the other holders of `active` alone;
 * `p0Bound` is the P0 bound in force (`legionP0Bound`). A daemon at the P0 start's contract entry writes both on
 * every state, and both are optional here because the route strips them for a caller below the entry
 * (`api.stateFor`, `withoutP0Start`). */
const legionAdmission = z.object({
  cap: z.number().int().nonnegative().optional(),
  p0Holders: z.number().int().nonnegative().optional(),
  p0Bound: legionP0Bound.optional(),
  active: z.array(nonEmptyString).optional(),
  waiting: z.array(nonEmptyString).optional(),
  trees: z.array(issueKey).optional(),
  blocked: z.array(legionBlockedIssue).optional(),
  held: z.array(legionHeldIssue).optional(),
  free: z.number().int().nonnegative().optional(),
});

/** `api.ControllerLocator` — the record of the session registered as the project's controller: the
 * runtime the daemon runs under (`tmux` or `kubernetes`, its configured runtime), the session, and
 * when it registered. `external` is `true` for the operator-launched controller and for the one the
 * daemon launches itself under `controller: daemon`, whose process is a claim like any other. */
const legionControllerLocator = z.object({
  runtime: z.enum(["tmux", "kubernetes"]).optional(),
  external: z.literal(true).optional(),
  sessionId: nonEmptyString.optional(),
  registeredAt: timestamp.optional(),
  /** 0 = no contract recorded. */
  pluginContract: z.number().int().nonnegative().optional(),
});

/** `api.AgentSecretsLoginView` — the daemon's own agent-secrets token login's current status
 * (runtime.kubernetes.agent_secrets): `state` is one of "none" (no login has finished yet),
 * "issued", or "failed" (the latest login won no credential). The daemon logs in with its own
 * ServiceAccount token, which no person approves, so there is no confirmation code (contract 22). */
const legionAgentSecretsLoginView = z.object({
  state: z.string().optional(),
});

/** `api.DispatchCredentialView` — the daemon's Dispatch credential under `dispatch_token_broker`
 * (contract 275): `session`, the session its own Dispatch calls run under
 * (`legion-daemon:<PROJECT>`); `transitionLaunches`, the launches since the daemon's boot that
 * carried the transition token of `dispatch_token_transition_file`, and `lastTransitionLaunchAt`,
 * when the latest of them was sent, null before the first; and `liveLaunchesWithDispatchToken`,
 * the live launches whose start carried a Dispatch token, whichever daemon started them. A
 * deployment removes `dispatch_token_transition_file` once the last is 0. Never a token. No plugin
 * code reads a field of it (`LEGION_ANSWER_READS` names none), so each is optional, and a value
 * that is present is still held to its type. */
const legionDispatchCredentialView = z.object({
  session: nonEmptyString.optional(),
  transitionLaunches: z.number().int().nonnegative().optional(),
  lastTransitionLaunchAt: timestamp.nullable().optional(),
  liveLaunchesWithDispatchToken: z.number().int().nonnegative().optional(),
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
 * alone decides) or `boot` (what boot measured), absent on every other row (contract 25), or, on
 * `repository-cache-room`, `caches`: what the repository caches' last health reads said of their
 * room (contract 213). A caller below 213 is served that row without `source`, since the enum it
 * was built with names two words: a value follows the contract the caller states, else its pod's
 * lowest recorded contract. A gap is reported here, never refused (contract 16). */
const legionCapabilityState = z.object({
  name: nonEmptyString.optional(),
  status: z.enum(["present", "unchecked", "withheld", "decided", "open"]).optional(),
  detail: nonEmptyString.optional(),
  decision: nonEmptyString.optional(),
  configLine: nonEmptyString.optional(),
  source: z.enum(["configuration", "boot", "caches"]).optional(),
});

/** `api.Settings` — the daemon's resolved legion.yaml with every credential, credential source and
 * credential file path left out (contract 35). Its keys are legion.yaml's own; the two the spec
 * names are typed, the rest ride as the daemon writes them, so a new legion.yaml key is no
 * contract change. `max_fix_attempts` was typed here until contract 59 removed the key, and
 * `review_round_cap` is the one cap left that blocks a pull request. */
const legionSettings = z.looseObject({
  review_round_cap: z.number().int().positive().optional(),
  worker_boot_timeout_seconds: z.number().int().positive().optional(),
});

/** `api.CacheView` — a repository cache as the Kubernetes runtime shows it: its repository and
 * Sandbox, its `state` (`failing` with the kubelet's `reason`, `refused` with its Service's),
 * whether its pod is Ready behind its own Service and its address then, from its last health read
 * the default branch's `head`, `lastFetchAt` and the `bytes` it serves or the read's
 * `healthError`, and, for a repository no live record names any more, `retainedUntil` (contract
 * 32). */
const legionCacheView = z.object({
  repo: nonEmptyString.optional(),
  sandbox: nonEmptyString.optional(),
  state: z.enum(["starting", "cloning", "ready", "failing", "refused"]).optional(),
  reason: nonEmptyString.optional(),
  ready: z.boolean().optional(),
  address: nonEmptyString.optional(),
  head: nonEmptyString.optional(),
  lastFetchAt: timestamp.optional(),
  bytes: z.number().int().positive().optional(),
  healthError: nonEmptyString.optional(),
  retainedUntil: timestamp.optional(),
});

/** `api.AppInstallationsView` — each role App's installation on one repository (contract 152):
 * the implement App a worker pushes and opens pull requests with, and the review App a reviewer
 * approves with. The controller's `read_state` summary reads both on each `live` entry
 * (`LEGION_ANSWER_READS`), and both stay optional: the daemon writes `apps` only once GitHub
 * answered both reads. */
const legionAppInstallations = z.object({
  implement: z.boolean().optional(),
  review: z.boolean().optional(),
});

/** `api.LiveRepositoryView` — one repository a live record names (contract 202): `repo`, the name
 * the records keep it under; `servedAs`, the other name of its configured pair
 * (`repository_renames`) where the rename rule serves it under that one; and, from what GitHub last
 * answered for the name it is served under, `apps`, each role App's installation on it once GitHub
 * answered both reads; `readError`, why GitHub's last read did not name it (a refusal such as a
 * 404, or a read that did not answer); and `cache`, its repository cache under the Kubernetes
 * runtime. The daemon writes `repo` on every entry and each other key only once it holds
 * something. What Dispatch answered of its webhook App is in the block's `withoutWebhookApp`
 * alone. The controller's `read_state` summary reads `repo`, `servedAs`, `apps` and `readError`
 * to print `legion state`'s line for each repository whose last read failed or that a role App is
 * not installed on (`LEGION_ANSWER_READS`), and every key stays optional, since the summary
 * leaves out what an entry does not carry. */
const legionLiveRepository = z.object({
  repo: nonEmptyString.optional(),
  servedAs: nonEmptyString.optional(),
  apps: legionAppInstallations.optional(),
  readError: nonEmptyString.optional(),
  cache: legionCacheView.optional(),
});

/** `api.WebhookAppGapView` — an entry of `withoutWebhookApp` (contract 43, kept in contract 152's
 * shape by 202): a live repository Dispatch's GitHub App is not known to be installed on, under
 * the name the records keep it under, with why (not installed, the check failed, or Dispatch does
 * not answer the check). The daemon works it all the same. No plugin code reads it
 * (`LEGION_ANSWER_READS`), so both keys are optional. */
const legionWebhookAppGap = z.object({
  repo: nonEmptyString.optional(),
  reason: nonEmptyString.optional(),
});

/** `api.RepositoriesView` — the repositories the daemon works in (contract 202): `live`, one entry
 * for each repository the project's live records name, read in the state's own snapshot and sorted
 * by its lowercased `<owner>/<name>`; `withoutWebhookApp`, each of those that Dispatch's GitHub App
 * is not known to be installed on, in `live`'s order; and `caches`, the repository caches of
 * repositories no live record names, each kept until its `retainedUntil` (contract 32). The daemon
 * writes all three on every body, never null; the controller's `read_state` summary reads `live`
 * and no plugin code reads the other two (`LEGION_ANSWER_READS`), so each is optional here. A
 * repository is served once GitHub answered that both role Apps are installed on it, whatever
 * names it.
 *
 * `withoutWebhookApp` is the block's one report of Dispatch's webhook App, and no `live` entry
 * carries a copy of it. It keeps contract 152's shape for every caller: the operator's `legion
 * state` states no contract and asks from no pod, so the daemon serves it this block, and a
 * `legion state` of 152 reads its webhook App lines from that list. A plugin of an earlier
 * contract up to 189 reads the block with that contract's own schema, which also requires
 * `owners`, `mapped` and `refused`, and one from 193 on reads every key of the block as optional
 * (main's #3659). The route serves every caller below 202 that older block all the same, written
 * from the same entries: the contract its plugin states, else the highest its pod's claims record
 * (`api.stateFor`, `withoutRepositoriesLive`). That body is never served to a plugin of this
 * contract, so this schema reads only this block. */
const legionRepositories = z.object({
  live: z.array(legionLiveRepository).optional(),
  withoutWebhookApp: z.array(legionWebhookAppGap).optional(),
  caches: z.array(legionCacheView).optional(),
});

/** `api.ConsumerLagView` — one durable consumer's lag, read from NATS after the state's
 * transaction commits (contract 43): its `name`; `waiting`, the messages it has not handled
 * (JetStream's pending plus acknowledgement-pending); `oldestAt`, the stream time of the oldest of
 * them, and `oldestAgeSeconds` its age at the read, both null when nothing waits or that message
 * could not be read; `lastHandledAt`, when the daemon last acknowledged one of its messages or
 * terminated one it gave up, null before the first; `stalled`, messages waiting and none handled for
 * 60 seconds; and `unavailable`, what NATS did not answer or refused, absent when every read
 * answered. */
const legionConsumerLag = z.object({
  name: nonEmptyString.optional(),
  waiting: z.number().int().nonnegative().optional(),
  oldestAt: timestamp.nullable().optional(),
  oldestAgeSeconds: z.number().int().nullable().optional(),
  lastHandledAt: timestamp.nullable().optional(),
  stalled: z.boolean().optional(),
  unavailable: nonEmptyString.optional(),
});

/** `record.IntakeGiveUpReason` — why the intake gave a message up: `bound`, JetStream delivered it
 * at least the bound's count of times and its first failure is at least the bound's hour old; `did
 * not end`, a delivery whose handling never ended, which the next delivery found once the bound was
 * reached; `retention`, the stream would delete it before its next delivery; `undecodable`, it did
 * not decode, given up at its first delivery; and `consumer moved past`, an open failure the
 * consumer moved past without delivering it again. */
const LEGION_INTAKE_GIVE_UP_REASONS = [
  "bound",
  "did not end",
  "retention",
  "undecodable",
  "consumer moved past",
] as const;

/** `api.IntakeGiveUpCountView` — one durable consumer's open give-ups (contract 228): its `name`,
 * how many are `open`, and `newestSeq`, the newest stream sequence among them, which a replay or a
 * discard of all of them names as its `throughSeq`. A consumer with none open is not listed. The
 * controller's `read_state` summary prints all three, so each is required (LEGION_ANSWER_READS). */
const legionIntakeGiveUpCount = z.object({
  name: nonEmptyString,
  open: z.number().int().positive(),
  newestSeq: z.number().int().positive(),
});

/** `api.IntakeGiveUpView` — one open give-up as the state lists it, without the message's headers
 * and payload, which the record keeps whole: its record `id`, the `consumer` that delivered it, its
 * `subject`, `streamSeq` and `storedAt`, the fact's `eventId`, null for a message that did not
 * decode, how many `deliveries` JetStream made of it, `gaveUpAt` and `reason`, and `lastError`,
 * the first 300 characters of its last error. No plugin code reads a listed give-up, so every
 * field is optional (LEGION_ANSWER_READS); `reason` is still read with its enumeration, which
 * refuses a word it does not name. */
const legionIntakeGiveUp = z.object({
  id: z.number().int().positive().optional(),
  consumer: nonEmptyString.optional(),
  subject: nonEmptyString.optional(),
  streamSeq: z.number().int().positive().optional(),
  storedAt: timestamp.optional(),
  eventId: nonEmptyString.nullable().optional(),
  deliveries: z.number().int().nonnegative().optional(),
  gaveUpAt: timestamp.optional(),
  reason: z.enum(LEGION_INTAKE_GIVE_UP_REASONS).optional(),
  lastError: z.string().optional(),
});

/** `api.IntakeGiveUpsView` — the intake's open give-ups (contract 228), the messages it gave up on
 * that no replay or discard has closed: `consumers`, each consumer with one open, by name, and
 * `newest`, the newest of them, latest given up first, at most 50. The daemon writes both lists
 * on every body, never null and empty while nothing is open. The controller's summary reads
 * `consumers`, which is required; `newest` is optional, since no plugin code reads it
 * (LEGION_ANSWER_READS). */
const legionIntakeGiveUps = z.object({
  consumers: z.array(legionIntakeGiveUpCount),
  newest: z.array(legionIntakeGiveUp).optional(),
});

/** What every view of a pause carries: its id, when it began, who paused, and why. Each is
 * required: the controller's `read_state` summary names all four of the state's pauses
 * (LEGION_ANSWER_READS), and a refusal that names a pause is a body no plugin code parses, which
 * keeps every field the daemon always writes. The pause and resume answers, which no plugin code
 * reads, carry the same fields as `legionAnsweredPause`. */
const pauseFields = {
  id: z.number().int().positive(),
  since: timestamp,
  by: legionPauseActor,
  reason: nonEmptyString,
};

/** `api.PauseView` — one pause of the project (contract 178): its id, `since`, when it began,
 * `by`, who paused, `reason`, why, and, once a resume has ended it, `endedAt` and `endedBy`, when
 * and by whom. A refusal that names the pause that stands carries this shape; the pause is one no
 * resume has ended, so it names no end. */
const legionPause = z.object({
  ...pauseFields,
  endedAt: timestamp.optional(),
  endedBy: legionPauseActor.optional(),
});

/** `api.PauseView` where the daemon writes only a pause a resume has ended: the state's
 * `lastPause`, and the last pause a refused resume names. `endedAt` and `endedBy` are both
 * required there, since the store holds an end only with who ended it
 * (`project_pauses_end_has_its_by` in the same migration), and the controller's summary names
 * both of the state's `lastPause`. */
const legionEndedPause = z.object({
  ...pauseFields,
  endedAt: timestamp,
  endedBy: legionPauseActor,
});

/** `api.PauseStopView` — one claim whose stop the open pause still waits on: the claim, its issue
 * and role, and `askedAt`, when the pause asked it to stop. A role in a turn stops when that turn
 * ends, so it is listed until then. The controller's summary names each by its issue and role,
 * so those two are required; `claim` and `askedAt` are optional, since no plugin code reads them
 * (LEGION_ANSWER_READS). */
const legionPauseStop = z.object({
  claim: nonEmptyString.optional(),
  issue: issueKey,
  role: z.enum(LEGION_ROLES),
  askedAt: timestamp.optional(),
});

/** `api.PauseHeldView` — one outbox row the open pause holds: its id, its kind, its issue,
 * `claim`, the claim it is for, absent for a row that names none (an architect's notice, which
 * the daemon routes when the row runs), and `waitedSeconds`, how long the row has been due. The
 * controller's summary counts the rows and reads no field of one, so every field is optional
 * (LEGION_ANSWER_READS). */
const legionPauseHeldRow = z.object({
  id: z.number().int().positive().optional(),
  kind: nonEmptyString.optional(),
  issue: nonEmptyString.optional(),
  claim: nonEmptyString.optional(),
  waitedSeconds: z.number().int().nonnegative().optional(),
});

/** `api.PausedView` — the project's open pause as the state shows it (contract 178): the pause,
 * `waitsOn`, the claims whose stop it still waits on, and `held`, every row it holds. The rows
 * are listed under the pause because the pause is the reason each one waits. Both lists are
 * never null, and both are required: the controller's summary reads each. */
const legionPaused = z.object({
  ...pauseFields,
  waitsOn: z.array(legionPauseStop),
  held: z.array(legionPauseHeldRow),
});

/** `api.State`, the body of `GET /legion/v1/state`. `controllerLocator` is absent until a session
 * registers as the project's controller: with the capability `legion controller start` fetched, or,
 * under `controller: daemon`, with the boot token of the daemon's own controller launch;
 * `agentSecretsLogin` is absent when the deployment configures no broker (contract 9);
 * `capabilities` is the deployment's report, never null (contract 16); `strandedClaims` is every
 * claim nothing will wake, by issue and then role, never null (contract 23); `heldOutboxRows` is
 * every due lifecycle outbox row held behind its issue's running one, by when it came due and then
 * id, never null (contract 28); `repositories` is the repositories the project's live records
 * name, the ones of them Dispatch's webhook App is not known to be installed on, and the caches of
 * the rest (contracts 31, 43, 202); `settings` is the daemon's resolved
 * legion.yaml with no credential, credential source or credential file path, required, and loose
 * like `payload` above, so a legion.yaml key added later is no contract change (contract 35);
 * being `z.looseObject` it keeps
 * the keys it does not name, where the shapes around it drop theirs; `consumers` is the lag
 * of the daemon's durable consumers, never null, empty for a daemon with no workflow (contract 43);
 * `wakesWaiting` is every wake the daemon's pacing is holding until its turn, in claim order, never
 * null, empty at every moment outside a restart or a refresh wave (contract 53). It is optional
 * here only because a daemon at this contract leaves it off one body: a reader before 53 refuses
 * the key outright, empty as surely as naming a wake, so the route strips it for a caller below
 * 53: the contract its plugin states, else the highest its pod's claims record (`api.stateFor`,
 * `withoutWakesWaiting`). A reader that finds it absent has been served that stripped body and
 * learns nothing about which wakes wait; it is never a daemon older than this plugin, whom the
 * boot gate and `legion probe-image` refuse. `claimsToRefresh` is how many claims the stale-plugin
 * lane still has to relaunch onto the daemon's plugin, a number and never null, 0 when the lane has
 * nothing left (contract 73). It is what `wakesWaiting` cannot say: that lane holds at most one
 * minute of the pacing at a time, so its waiting wakes are what it has admitted and this is what it
 * has left. It is optional for the same reason `wakesWaiting` is and no other: the route strips it
 * for a caller below 73, read the same way, since a reader before 73 refuses the key when it is 0
 * as surely as when it names a number (`api.stateFor`, `withoutClaimsToRefresh`). A caller that
 * states no contract and whose pod's claims record none, or that the daemon can place in no pod,
 * is served the count. The two strips compose on one body: a caller below 53 is below 73 too
 * and is served neither key. `contract` is the mark of the narrowing, the two contract numbers
 * the daemon's own log line names (contract 75), optional for the same reason the other two are:
 * the route strips it for a caller below 75, read the same way, since a reader before 75 refuses
 * the key (`api.stateFor`, `withoutContractMark`). A caller that states no contract and whose
 * pod's claims record none, or that the daemon can place in no pod, is served the mark, and the
 * shapes `internal/projection` answers before the route marks them carry none either.
 * `paused` is the project's open pause, with the claims whose stop it still waits on and the rows
 * it holds, and `lastPause` is its newest ended pause, with when and by whom it was ended
 * (contract 178). The daemon writes each only where there is one: `paused` while the project is
 * paused and `lastPause` while it runs after a pause has ended, so a state carries one of them at
 * most, and neither before the project's first pause. Both are optional for that reason, and for
 * the one `wakesWaiting` is optional for: the route strips both for a caller below 178, read the
 * same way (`api.stateFor`, `withoutPause`), so a reader below 178 learns nothing of the pause
 * from a state that carries neither. `intakeGiveUps` is the intake's open give-ups, the messages
 * it gave up on that no replay or discard has closed, counted by consumer and the newest of them
 * listed (contract 228). The daemon writes it on every body, its two lists empty while nothing is
 * open, and it is optional for the reason `wakesWaiting` is: the route strips it for a caller
 * below 228, read the same way (`api.stateFor`, `withoutIntakeGiveUps`), since a reader before
 * 228 refuses the key whether its lists are empty or not.
 * `dispatchCredential` is the daemon's Dispatch credential and the two counts of the Dispatch
 * tokens its launches hand out, written only under `dispatch_token_broker` (contract 275); the
 * route strips it for a caller below 275, read the same way (`api.stateFor`,
 * `withoutDispatchCredential`). */
export const LegionStateResponse = z.object({
  daemon: daemonInfo.optional(),
  contract: legionContract.optional(),
  admission: legionAdmission.optional(),
  issues: z.record(z.string(), legionIssue),
  pendingStatusWrites: z.array(legionPendingStatusWrite).optional(),
  heldOutboxRows: z.array(legionHeldOutboxRow).optional(),
  strandedClaims: z.array(legionStrandedClaim).optional(),
  wakesWaiting: z.array(legionWakeWaiting).optional(),
  claimsToRefresh: z.number().int().nonnegative().optional(),
  paused: legionPaused.optional(),
  lastPause: legionEndedPause.optional(),
  controllerLocator: legionControllerLocator.optional(),
  agentSecretsLogin: legionAgentSecretsLoginView.optional(),
  dispatchCredential: legionDispatchCredentialView.optional(),
  capabilities: z.array(legionCapabilityState).optional(),
  repositories: legionRepositories.optional(),
  settings: legionSettings.optional(),
  consumers: z.array(legionConsumerLag).optional(),
  // `lowestPluginContract` is the lowest contract among the claims of the project that hold a
  // process and record one, absent where no claim does (contract 193). No plugin code reads the
  // value — it is a number for an operator and for the serving-down's own reckoning — so it is
  // optional here, and a reader that finds it absent learns that nothing of the project runs a
  // plugin it knows the contract of.
  lowestPluginContract: z.number().int().positive().optional(),
  intakeGiveUps: legionIntakeGiveUps.optional(),
});

export type LegionState = z.output<typeof LegionStateResponse>;
export type LegionIssue = LegionState["issues"][string];

/** `api.Issue` again, as the body of `GET /legion/v1/issues/{key}/record` (contract 193): one
 * issue's record, equal field for field to the value the whole-project body holds under
 * `issues.<key>`. It is the very schema `LegionStateResponse` reads that value with, so the two
 * readers cannot drift apart. The daemon answers 404 with the code `RECORD_NOT_FOUND` for an
 * issue it records nothing of, which is the client's absent-record branch and no body at all. */
export const LegionIssueRecordResponse = legionIssue;

/** `api.Health`, the body of `GET /healthz` (contract 193): that the daemon is serving, and the
 * project it serves. `project` is required because the controller's boot check reads it
 * (`packages/pi-legion/src/controller-session.ts`), which is the one word that read the whole
 * state before. `status` is a word for an operator: no plugin code takes its value. */
export const LegionHealthResponse = z.object({
  status: nonEmptyString.optional(),
  project: nonEmptyString,
});

export type LegionHealth = z.output<typeof LegionHealthResponse>;

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
 * provider said about a refusal, and a clean turn carries none.
 *
 * `httpStatus` is the HTTP status the provider refused the turn with, where the producer's
 * provider library gives one (Oh My Pi's `errorStatus` on the refused assistant message, Claude
 * Code's `api_error_status`); a refusal without it is one whose status cannot be read, and the
 * daemon reads no status out of `message`. This schema admits 100 to 599 alone; a daemon that is
 * sent another number takes it as no status and warns. `unprompted: true` says the turn ran no
 * prompt the daemon sent — a phase reminder, an Envoy message, a background job's result — so no
 * prompt result comes for it and the daemon judges it from this report: a refusal whose status
 * cannot clear (a 4xx other than 401, 403, 408 and 429) fails the claim, and any other refusal
 * counts in the claim's run of provider-error turns. A refused report without it is judged by the
 * result of the prompt its turn ran, never by itself, and a clean report, with it or without, ends
 * that run when no later turn's report has reached the daemon before it
 * (`claim.TurnOutcomeRequest`). `code` is the class the producer gave a refused turn with the
 * host's own classifier (Oh My Pi's `isCyberPolicyRefusal`, the reading that sets a prompt
 * result's `code`): `cyber_policy` alone, OpenAI's refusal of the session's content, which fails
 * an `unprompted` turn's claim at once under cause `cyber_policy`, whatever its status. A daemon
 * before contract entry 20261125174608 (cyber-policy) refuses a body that carries it. */
const turnOutcomeFields = {
  generation: z.number().int().positive(),
  sequence: z.number().int().positive(),
  outcome: z.enum(["clean", "refused"]),
  message: z.string().optional(),
  httpStatus: z.number().int().min(100).max(599).optional(),
  unprompted: z.boolean().optional(),
  code: z.literal("cyber_policy").optional(),
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
 * turns the claim's reports now end with, counted up to `launch_failure_limit`, which the daemon
 * answers each report with and which decides nothing. `failed` is whether this report failed the
 * claim: a run of refused turns fails none (dispatch://INFRA-4508), and the one kind of report
 * that does is that of an `unprompted` turn the provider refused with a status that cannot clear,
 * or that carries `code: "cyber_policy"` whatever its status, which holds the issue for its
 * architect as such a prompt's result does. This and `claim.TurnOutcomeResponse` are where that
 * rule is stated; every other reader of `failed` points here. The producer acts on neither field —
 * its turn is over — and logs them, which is where an operator sees the count being made. */
export const LegionTurnOutcomeResponse = z.object({
  refusedRun: z.number().int().nonnegative(),
  failed: z.boolean(),
});

export type LegionTurnOutcome = z.output<typeof LegionTurnOutcomeResponse>;

/** `claim.ReadyForUpdateRequest`, the body of `POST /legion/v1/claims/ready-for-update`: the
 * worker's answer to its pod's update message, which says it has pushed everything it wants kept
 * and its process may be replaced. `round` is the round the worker answers for, as the update
 * message it read named it: the daemon takes the answer only for the round its claim stands in,
 * and refuses one naming any other round with 409 and a sentence naming both. `head` is the
 * commit the worker reports as the head of `legion/<issue>@origin`, empty or absent when the
 * branch is not there (a planner step that runs before the branch's first push); the daemon
 * records it and verifies nothing of it. */
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
 * of the git bundle of unpushed work it saved, zero when the workspace held none. `roles` is
 * required because `read_release_records` reads it: the empty list is the whole mark of a
 * release no wake can reach, which that report counts and measures nowhere, and the daemon
 * writes the list on every release, empty rather than null. `issue` is required because that
 * report reads it too: a wake a project pause held names no release, and ends the releases of its
 * issue instead. */
const legionReleaseRecord = z.object({
  id: z.number().int().positive(),
  issue: issueKey,
  generation: z.number().int().nonnegative().optional(),
  roles: z.array(nonEmptyString),
  at: timestamp,
  bundleBytes: z.number().int().nonnegative().optional(),
});

/** `api.ResumeRecord` — one wake of a released pod and what it cost: the release it ended (0 when
 * no release row is recorded for it), the role it was for, what woke it, when the wake reached the
 * daemon, and `podReadyAt` and `firstTurnAt`, each absent until it is observed. `stuck` is a wake
 * that went ten minutes with no turn of the woken role. `pause` is the project pause that held
 * the wake (contract 178), absent on a wake no pause held: a held wake pairs with no release, so
 * its `release` is 0, and its time to the first turn measures the pause, so a reader of these
 * records counts it apart (`releaseRecords` in `packages/pi-shared/src/legion-phase-ops.ts`).
 * That report keeps such a wake under its `issue`, which is required for that reason, and reads
 * `pause` where the daemon writes it. */
const legionResumeRecord = z.object({
  id: z.number().int().positive().optional(),
  release: z.number().int().nonnegative(),
  issue: issueKey,
  role: nonEmptyString.optional(),
  kind: nonEmptyString,
  at: timestamp,
  podReadyAt: timestamp.optional(),
  firstTurnAt: timestamp.optional(),
  stuck: z.boolean().optional(),
  pause: z.number().int().positive().optional(),
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
  id: z.number().int().positive().optional(),
  issue: issueKey.optional(),
  reason: nonEmptyString,
  detail: nonEmptyString.optional(),
  firstAt: timestamp.optional(),
  at: timestamp.optional(),
  passes: z.number().int().positive().optional(),
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
  role: z.literal("controller").optional(),
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
 * the review App's read-only token for every GitHub owner that App is installed on, and for each
 * owner a configured rename moved a repository away from, each rendered as the two
 * files gh reads (`ghconfig.Rendered`) and keyed by lowercased owner, the owner whose directory
 * `GH_CONFIG_DIR` names, the soonest of the tokens' expiries, `renames`, the configured pairs the
 * bundle routes a moved repository by, each `<owner>/<name>` as GitHub spells it, which a project
 * configuring none answers without, and `installed`, the owner accounts the review App is
 * installed on, lowercased and sorted, read with the bundle (contract 152).
 * `legion controller start` writes the files under `<state_dir>/gh/<owner>`, refreshes them every
 * minute, and tells its controller which owners its App is installed on from `installed`: `owners`
 * cannot say it, since a bundle also holds a directory for an owner a configured rename moved a
 * repository away from. The plugin never reads it. */
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
  renames: z.array(z.object({ from: nonEmptyString, to: nonEmptyString })).optional(),
  installed: z.array(nonEmptyString),
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
 * the claim's phase and its pending task until the next wake for its role; an `operator` and a
 * `controller` one hold against the daemon's rules, ranked in that order: the controller's
 * `resume_agent` lifts its own and never the operator's (the agent-actions contract). */
const legionSuspendedBy = z.enum([
  "unknown",
  "operator",
  "controller",
  "daemon",
  "exit",
  "released",
]);

/** `api.AgentActionResponse`, what four of the controller's actions on an agent (stop, retry,
 * suspend, resume_agent, compact and fresh session of one agent, named by its issue and role)
 * answer: stop, retry, suspend and resume
 * (`POST /legion/v1/agents/stop|retry|suspend|resume`, each taking `LegionClaimRequest`, the
 * agent-actions contract): the claim's `state` after the request — `retired` after a stop; `suspended` after a
 * suspension that landed, and still `working` or `launching` with
 * `suspensionHeld` after one held for the agent's turn or its ready; `launching` after a resume or
 * a retry the claim's machine ran; and still `failed` or `retired` after a retry of a phase worker
 * whose start its issue's phase queued. `suspendedBy` (while the claim is suspended) and
 * `suspensionHeldBy` (while one is held) say who holds the suspension: `controller` for the
 * controller's own, which its `resume_agent` and the operator's resume lift; and `operator` for
 * the operator's, which only the operator lifts. These
 * routes are the one session-facing surface that names a suspender, since no plugin below
 * the agent-actions contract calls them; the state does not ("no session-facing response carries suspendedBy"
 * in legion-api.test.ts). The `legion` tool hands the answer to its agent whole, which is no
 * read, so the reader takes every field as optional (`LEGION_ANSWER_READS`), `state` among them,
 * though the daemon always writes it. The three enumerations are the entry's enumerated rows
 * (`enumeratedRows` of `contracts/20261125182642_agent-actions.json`), each whole at the
 * agent-actions contract. */
export const LegionAgentActionResponse = z.object({
  state: z.enum(LEGION_CLAIM_STATES).optional(),
  suspendedBy: legionSuspendedBy.optional(),
  suspensionHeld: z.literal(true).optional(),
  suspensionHeldBy: legionSuspendedBy.optional(),
});

/** `api.PodUpdateRequest`, the project's controller asking the daemon to replace the pod one issue
 * of its project runs in (`POST /legion/v1/issues/pod-update`, the agent-actions contract): its grant and the
 * issue. The daemon opens its update round over that pod as a drift opens one, and over a pod an
 * earlier round kept as well: each role of the pod is asked to push its work and answer
 * `ready_for_update`, and the round reaches its end once each has answered, no turn runs and each
 * role's report since its last turn names no running background work, or at the round's bound
 * (`pod_update_grace_minutes`). The daemon then reads the pod's workspace and replaces the pod only
 * where that read finds nothing that no branch or tag on origin holds; where it finds such work, or
 * fails, the round keeps the pod and ends with no replacement. It takes a controller
 * grant alone (`CONTROLLER_REQUIRED`). An issue of another project, or one with no claim the daemon
 * supervises, is `NO_CLAIM`; one whose claims run no worker in a pod is `NO_POD`; one whose pod is
 * being replaced already is `POD_CHANGING`; an opening a limit of the project holds back is
 * `POD_UPDATE_LIMITED`, whose message names the limit; and a round no claim records within the
 * daemon's wait is `POD_UPDATE_NOT_RECORDED` (503), since a 200 is only ever a round a claim
 * records in the daemon's store, which the daemon's sweeps carry across a restart. A request that
 * waits more than the daemon's bound (30 s) for a sweep's walk over the project's issues, or ends
 * before it, opens nothing and is `POD_UPDATE_BUSY` (503, with `Retry-After`, whose wait its
 * sentence names). While the project is paused it is `PROJECT_PAUSED` (409, with the `pause` that
 * stands). A pod a pause holds is `POD_HELD_BY_PAUSE` (409): the hold of a pause that began after
 * the daemon read the project as running, or a hold the resume left on a pod none of whose roles
 * is live yet. Neither opens a round. */
export const LegionPodUpdateRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
});

/** `api.PodUpdateResponse`, what the controller's pod update answers: the `round` over the issue's
 * pod (`pod:<pod uid>`), the `reasons` it records, which its ask to each role states, and whether
 * this request `opened` it — false for a round already open over the pod, a drift's or an earlier
 * request's, which the request leaves as it stands. The `legion` tool's `replace_pod` hands the
 * answer to its agent whole, which is no read, so the reader takes every field as optional
 * (`LEGION_ANSWER_READS`), though the daemon always writes all three. */
export const LegionPodUpdateResponse = z.object({
  round: nonEmptyString.optional(),
  reasons: z.string().optional(),
  opened: z.boolean().optional(),
});

/** `api.OperatorClaim`, the body of the operator routes that act on one claim (`POST
 * /legion/v1/operator/claims`, `…/{token}/deliver|suspend|resume|stop|retry|forget-session|close`):
 * the claim as its supervisor holds it, but its hashes. `forgottenSession` and
 * `forgottenSessionFile` appear only in a successful `forget-session` response, and
 * `phaseNotRestarted` only in a `retry` that asked for a fresh session. The Go CLI reads this
 * answer and the plugin's client parses it nowhere, so a field the daemon always writes stays
 * required here and the read-set census never reaches it.
 */
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
   * two causes answer separate questions and never overwrite each other. `lastDeathAt` and
   * `lastStartFailureAt` (contract 256) are when the daemon recorded the cause beside each, absent
   * when that cause is and beside a cause a daemon before 256 recorded, except after a rollback
   * (the daemon's migration 20261124073512's header). `lastDeathAt` dates the
   * death: one before `lastReady` is a death the running agent survived. The daemon drops the
   * start failure, with its time, when the agent says it is ready.
   * `legion claims list --json` serves them. */
  lastDeathCause: nonEmptyString.optional(),
  lastDeathAt: timestamp.optional(),
  lastStartFailure: nonEmptyString.optional(),
  lastStartFailureAt: timestamp.optional(),
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
 * the daemon's last pod-update sweep decided beside the claims. The Go CLI reads this answer and
 * the plugin's client parses it nowhere, so its fields stay required. */
export const LegionOperatorClaimsResponse = z.object({
  claims: z.array(LegionOperatorClaimResponse),
  pod_update_rounds: legionPodUpdateRounds,
});

/** The credential grant body from `internal/api/credentials.go`. */
export const LegionGrantResponse = z.object({
  grantId: nonEmptyString,
  expiresAt: timestamp.optional(),
});
export type LegionGrant = z.output<typeof LegionGrantResponse>;

/** Every completed fact route returns an intentional empty JSON object. The shape names no field,
 * so it accepts any JSON object and refuses a body that is not one (an array, a string); a field
 * the daemon adds to a fact route is caught by the golden drift test in `legion-api.test.ts`,
 * where each fact-route golden is `{}`, not at runtime. */
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
  released: z.array(nonEmptyString).optional(),
  started: z.array(issueKey).optional(),
  held: z.array(legionBlockedIssue).optional(),
});

/** `api.PauseView` as the pause and resume answers carry it (contract 178): the pause a pause
 * began, or the pause a resume ended, with `endedAt` and `endedBy`, when and by whom. The
 * controller's `pause` and `resume` hand the daemon's answer to its agent whole
 * (`pi-legion/src/tools.ts`), and a value handed on whole is no read, so every field is optional
 * (LEGION_ANSWER_READS). Who acted is `legionPauseActor`, the state's own view: its
 * discriminator is required as every union's is, and its `kind` and a controller's `session`
 * are named in the read set at these paths as at the state's, since the census matches a read by
 * the field's name under its parent (`by.kind`, `by.session`, `endedBy.kind`,
 * `endedBy.session`), so the session stays required here too. */
const legionAnsweredPause = z.object({
  id: z.number().int().positive().optional(),
  since: timestamp.optional(),
  by: legionPauseActor.optional(),
  reason: nonEmptyString.optional(),
  endedAt: timestamp.optional(),
  endedBy: legionPauseActor.optional(),
});

/** `api.PlanRegisterResponse`, the plan register_plan took (contract 219): the version given and
 * each of its tasks, its id and the steps its block names as the plan writes them (`role` or
 * `role:key`). A task whose block names none runs the project's default steps, which the workflow
 * reads when the task's work starts, so it is answered with no step: an empty list, never null.
 * From the contract entry 20261125174555 (running-step) a task that runs also carries `running` (a list of `api.RunningStep`): one
 * entry, the act the task stands at once the version is taken. It names its `kind` (`step`,
 * `retro` or `merge`), its `place` (from 1 for a step, 0 for the retro and the merge) and the
 * `role` whose agent works it. A later version can change only the text of a running task, and
 * that agent reads no new start of it, so it is the agent the owner sends a message to for a
 * decision about the act that runs now. A plan whose content the daemon does not take is answered
 * `409 PLAN_REFUSED`, whose `error` holds every reason in order; the body's `reasons` lists them by
 * task, which `LegionErrorResponse` strips. An issue whose experiment's group runs the project's
 * default steps takes only a version that changes nothing but the text of its running task
 * `default`. Every other plan of it is answered `409 PLAN_NOT_TAKEN` (contract 220), a sentence
 * naming the group and no reasons. The reader takes every field as optional, since no plugin code
 * reads one: the `legion` tool hands the answer to its agent whole, so a field this schema leaves
 * out never reaches the agent. */
export const LegionPlanRegisterResponse = z.object({
  plan: z
    .object({
      version: z.number().int().positive().optional(),
      tasks: z
        .array(
          z.object({
            id: nonEmptyString.optional(),
            steps: z.array(nonEmptyString).optional(),
            running: z
              .array(
                z.object({
                  kind: nonEmptyString.optional(),
                  place: z.number().int().nonnegative().optional(),
                  role: nonEmptyString.optional(),
                })
              )
              .optional(),
          })
        )
        .optional(),
    })
    .optional(),
});
/** `api.ProjectPauseResponse`, the answer to a pause of the project, the controller's `POST
 * /legion/v1/project/pause` and the operator's `POST /legion/v1/operator/project/pause` alike:
 * the pause it began. The reader takes it as optional, since no plugin code reads it
 * (`legionAnsweredPause`). */
export const LegionProjectPauseResponse = z.object({
  pause: legionAnsweredPause.optional(),
});

/** `api.ProjectResumeResponse`, the answer to a resume of the project, the controller's `POST
 * /legion/v1/project/resume` and the operator's `POST /legion/v1/operator/project/resume` alike:
 * the pause it ended, and `released`, how many held rows it let out to run. The daemon's log
 * names those rows one by one. The reader takes both as optional, since no plugin code reads
 * them (`legionAnsweredPause`). */
export const LegionProjectResumeResponse = z.object({
  pause: legionAnsweredPause.optional(),
  released: z.number().int().nonnegative().optional(),
});

/** `api.P0BoundView`, the answer to the controller's set or clear of the P0 bound (the P0 start's contract entry),
 * `POST /legion/v1/project/p0-bound` and its `/clear`, and to the operator's two routes alike: the
 * bound in force, legion.yaml's and the live value that stands (`legionP0Bound`). The controller's
 * `set_p0_bound` and `clear_p0_bound` hand the answer to the agent whole (`pi-legion/src/tools.ts`). */
export const LegionP0BoundResponse = legionP0Bound;

/** `api.PauseFailure`, the refusal of a pause, of a resume, and of a request that would start a
 * turn while the project is paused: the code and the sentence every workflow refusal carries,
 * and what this one names beside them. `PAUSE_UNAVAILABLE` names the `setting` that turns the
 * pause off, and none from a daemon that runs no workflow; `PROJECT_ALREADY_PAUSED` and
 * `PROJECT_PAUSED` name the `pause` that stands; `PROJECT_NOT_PAUSED` names the `lastPause`,
 * where one has ended. A blank reason's refusal (`PAUSE_REASON_REQUIRED`) names nothing beside
 * its sentence and is the plain refusal (`LegionErrorResponse`). The daemon client reads a
 * refusal's code and sentence alone (`LegionErrorResponse`), and the sentence says which pause it
 * is, so this schema is what holds the rest of the body to the daemon's goldens. */
export const LegionPauseFailureResponse = z.object({
  code: nonEmptyString,
  error: nonEmptyString,
  setting: nonEmptyString.optional(),
  pause: legionPause.optional(),
  lastPause: legionEndedPause.optional(),
});

/** `api.ReplayedGiveUpView`'s `outcome` — how one give-up's replay ended: its fact `applied`, its
 * fact was applied already (`already applied`), the message holds `no fact`, each of which closes
 * the record, or it `failed`, which leaves the record as it stood, open or discarded, keeping the
 * new error. */
const LEGION_INTAKE_REPLAY_OUTCOMES = ["applied", "already applied", "no fact", "failed"] as const;

/** `api.IntakeReplayResponse`, the answer to a replay of give-ups, the controller's `POST
 * /legion/v1/intake/replay` and the operator's `POST /legion/v1/operator/intake/replay` alike
 * (contract 228): `replayed`, each record the replay took, in stream order, with its id, consumer,
 * stream sequence and outcome, and for `failed` the `error`, which the record keeps as its last
 * error; and `remaining`, the open records of the selection left after it, which another call
 * takes. The controller's `replay_give_up` hands the daemon's answer to its agent whole
 * (`pi-legion/src/tools.ts`), and a value handed on whole is no read, so every field is optional
 * (LEGION_ANSWER_READS); `outcome` is still read with its enumeration. */
export const LegionIntakeReplayResponse = z.object({
  replayed: z
    .array(
      z.object({
        id: z.number().int().positive().optional(),
        consumer: nonEmptyString.optional(),
        streamSeq: z.number().int().positive().optional(),
        outcome: z.enum(LEGION_INTAKE_REPLAY_OUTCOMES).optional(),
        error: nonEmptyString.optional(),
      })
    )
    .optional(),
  remaining: z.number().int().nonnegative().optional(),
});

export type LegionIntakeReplay = z.output<typeof LegionIntakeReplayResponse>;

/** `api.IntakeDiscardResponse`, the answer to a discard of give-ups, the controller's `POST
 * /legion/v1/intake/discard` and the operator's `POST /legion/v1/operator/intake/discard` alike
 * (contract 228): `discarded`, the id of each record it closed, in stream order. The controller's
 * `discard_give_up` hands it to its agent whole, so the field is optional, as the replay's are. */
export const LegionIntakeDiscardResponse = z.object({
  discarded: z.array(z.number().int().positive()).optional(),
});

export type LegionIntakeDiscard = z.output<typeof LegionIntakeDiscardResponse>;

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

/** The scale of a plan's rating and the most characters of its reason, defined once in
 * `legion-roles.ts` and re-exported here beside the request schema that holds a completion to
 * them. */
export { LEGION_PLAN_RATING_LEAST, LEGION_PLAN_RATING_MOST, LEGION_PLAN_REASON_MAX };

/** A plan rating's scale on `rating`, a number schema of the reader's: a whole number from
 * `LEGION_PLAN_RATING_LEAST` to `LEGION_PLAN_RATING_MOST`, each check refused with `refusal` where
 * one is given. The request schema below and the `legion` tool's `plan_rating` both build theirs
 * with it, so the scale is written once. */
export function legionPlanRatingScale(rating: z.ZodNumber, refusal?: string): z.ZodNumber {
  const error = refusal === undefined ? undefined : { error: refusal };
  return rating.int(error).min(LEGION_PLAN_RATING_LEAST, error).max(LEGION_PLAN_RATING_MOST, error);
}

/** A plan rating's reason on `reason`, a string schema of the reader's: not blank as the daemon's
 * strings.TrimSpace reads it (`goBlankPattern`), which is not JavaScript's trim, and at most
 * `LEGION_PLAN_REASON_MAX` characters counted as code points, as the daemon counts them, never as
 * a string's UTF-16 `length`. A blank reason is refused with `refusals.blank` alone, a longer one
 * with `refusals.long`. The request schema below and the `legion` tool's `plan_reason` both build
 * theirs with it, so the two rules are written once. */
export function legionPlanReasonRules(
  reason: z.ZodString,
  refusals: { readonly blank: string; readonly long: string }
): z.ZodString {
  return reason
    .refine((text) => !goBlankPattern.test(text), { error: refusals.blank, abort: true })
    .refine((text) => [...text].length <= LEGION_PLAN_REASON_MAX, { error: refusals.long });
}

/** `planReason`: why the retro rated the plan as it did (`legionPlanReasonRules`). */
const legionPlanReason = legionPlanReasonRules(z.string(), {
  blank: "planReason says why",
  long: `planReason holds at most ${LEGION_PLAN_REASON_MAX} characters`,
});

/** Which field of a plan's rating came alone: `"rating"` for a rating with no reason, `"reason"`
 * for a reason with no rating, and `undefined` for both or neither. The daemon takes the two only
 * together (`PLAN_RATING_INPUT`). The request schema below refuses either lone field, and the
 * `legion` tool's `handoff_complete` refuses each by its name, both by this, so the pair rule is
 * written once. */
export function legionPlanRatingLoneField(
  rating: number | undefined,
  reason: string | undefined
): "rating" | "reason" | undefined {
  if (rating !== undefined && reason === undefined) return "rating";
  if (reason !== undefined && rating === undefined) return "reason";
  return undefined;
}

/** `api.HandoffCompleteRequest`, the observation one worker reports to the workflow: a phase
 * completion, or a production check's look report when `checkOpenedAt` names that check. The
 * field alone selects the call, on any phase; absent it, the completion is a phase's.
 * A file-backed phase (planning, implementing, testing, reviewing) ends with a handoff record on
 * the Dispatch issue, which the worker writes with `dispatch handoff-write` under its own session;
 * its completion names it, `handoffId` and `handoffVersion`, which the daemon reads back from
 * Dispatch and refuses when it is missing (`HANDOFF_RECORD_REQUIRED`), unknown
 * (`HANDOFF_RECORD_NOT_FOUND`), unreadable (`DISPATCH_READ_FAILED`), of another issue, phase or
 * version (`HANDOFF_RECORD_MISMATCH`), or written by another session than the claim's
 * (`HANDOFF_SESSION_MISMATCH`). `commit` on such a completion is the head that record's body
 * names (`head`, the 40-character lower-case commit the work the handoff reports was done
 * against), which is what a completion of an earlier phase entry is taken at. The plugin refuses
 * the implementer's completion before it goes when its `implement` record names no head, when the
 * commit its workspace stands on (`@-`) is not on `legion/<issue>@origin`, and when the record's
 * head is not that pushed `@-`; a record naming no head leaves the planner's, the tester's and
 * the reviewer's completion without one. Retro and every completion of a phase its
 * role does not work report the commit the workspace stands on instead, the merger's reports the
 * head the merger merged, and a check report carries neither commit nor record (the look pushed
 * nothing), so all three are optional here, and the daemon requires each where it applies.
 * A named check report with any of these phase fields is refused (`CHECK_PHASE_FIELDS_UNEXPECTED`).
 * `verdict` is `pass`
 * or `fail` for the tester's completion, `pass`, `pending` or `fail` for a check report
 * (`CHECK_VERDICT_REQUIRED`), and the empty string for every other completion; it stays a string
 * on the wire, as the Go field is. `wakeAt` is the check report's alone (contract 54): when the
 * checking agent asks to be woken for its next look, which a pending report on a look that is not
 * the check's last must name (`CHECK_WAKE_REQUIRED`) and which no other verdict and no phase
 * completion may carry (`CHECK_WAKE_UNEXPECTED`). It is a `LegionInstant`, not a `timestamp`:
 * the daemon's decode of it refuses a time with no seconds, and so does this. `planRating` and
 * `planReason` are the rating of the plan the issue ran (contract 168), which the completion that
 * ends the retro carries and no other completion does (`PLAN_RATING_UNEXPECTED`): a whole number
 * from 1 (the plan cost the work) to 5 (the plan fit the work), and why, the two together
 * (`PLAN_RATING_INPUT`). A plugin at contract 168 or later must send both on its retro
 * (`PLAN_RATING_REQUIRED`). `returnTo` (contract 219) is the step a step that failed names for its
 * task to return to, by its place in the issue's running task, 1 for its first: a whole number of
 * 1 or more (`INVALID_RETURN_TO`), carried only by a completion with the verdict `fail`
 * (`RETURN_TO_NEEDS_FAIL`, which the daemon also answers a production check's report that names
 * one, whatever its verdict). Which step the task then runs is the workflow's to decide. */
export const LegionHandoffCompleteRequest = z
  .strictObject({
    grantId: nonEmptyString,
    summary: nonEmptyString,
    verdict: z.string(),
    commit: nonEmptyString.optional(),
    handoffId: nonEmptyString.optional(),
    handoffVersion: z.number().int().positive().optional(),
    checkOpenedAt: LegionInstant.optional(),
    wakeAt: LegionInstant.optional(),
    planRating: legionPlanRatingScale(z.number()).optional(),
    planReason: legionPlanReason.optional(),
    returnTo: z.number().int().positive().optional(),
  })
  .refine(
    (request) => legionPlanRatingLoneField(request.planRating, request.planReason) === undefined,
    { message: "planRating and planReason appear together" }
  )
  .refine((request) => request.returnTo === undefined || request.verdict === "fail", {
    message: "returnTo is carried only by a completion with the verdict fail",
  })
  .refine(
    (request) =>
      request.checkOpenedAt === undefined ||
      (request.commit === undefined &&
        request.handoffId === undefined &&
        request.handoffVersion === undefined),
    {
      message: "a report naming checkOpenedAt carries no commit, handoffId or handoffVersion",
    }
  );

/** `api.IssueStatusRequest`, the controller's explicit board-status write. */
export const LegionIssueStatusRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  status: z.enum(["todo", "backlog", "icebox"]),
});

/** `api.ProjectPauseRequest`, the controller's pause of the project: its grant, and why the
 * project is paused. It names no project: the daemon pauses its own. The daemon refuses a grant
 * that is not a controller's (`CONTROLLER_REQUIRED`), a blank reason (`PAUSE_REASON_REQUIRED`),
 * a deployment whose configuration cannot hold a pause (`PAUSE_UNAVAILABLE`) and a project that
 * is paused already (`PROJECT_ALREADY_PAUSED`). */
export const LegionProjectPauseRequest = z.strictObject({
  grantId: nonEmptyString,
  reason: nonEmptyString,
});

/** `api.ProjectResumeRequest`, the controller's resume of the project: its grant. It ends the
 * project's pause whoever made it, the operator's included, and the daemon refuses it for a
 * project that is not paused (`PROJECT_NOT_PAUSED`). */
export const LegionProjectResumeRequest = z.strictObject({
  grantId: nonEmptyString,
});

/** `api.ProjectP0BoundRequest`, the controller's set of the P0 bound (the P0 start's contract entry): its grant,
 * and exactly one of `bound`, a whole number from 1 to 2147483647, and `unbounded: true`, no bound
 * whatever legion.yaml says, and `reason`, why. It names no project: the daemon changes its own.
 * The daemon refuses a grant that is not a controller's (`CONTROLLER_REQUIRED`), a set that names
 * neither, both or a bound below 1 or above 2147483647 (`INVALID_P0_BOUND`), a blank reason
 * (`P0_BOUND_REASON_REQUIRED`) and a daemon with no workflow (`P0_BOUND_UNAVAILABLE`). */
export const LegionProjectP0BoundRequest = z.union([
  z.strictObject({
    grantId: nonEmptyString,
    bound: LegionP0BoundNumber,
    reason: nonEmptyString,
  }),
  z.strictObject({ grantId: nonEmptyString, unbounded: z.literal(true), reason: nonEmptyString }),
]);

/** `api.ProjectP0BoundClearRequest`, the controller's clear of the P0 bound (the P0 start's contract entry), which
 * returns the daemon to legion.yaml's `admission_p0_bound`: its grant and why. */
export const LegionProjectP0BoundClearRequest = z.strictObject({
  grantId: nonEmptyString,
  reason: nonEmptyString,
});

/** The two forms of `api.IntakeSelection`, the give-ups a replay or a discard takes: `giveUp`, one
 * record by its id, or `consumer` with `throughSeq`, every open record of that consumer at or below
 * that stream sequence. The state's `intakeGiveUps` gives both: each listed record's id, and each
 * consumer's newest sequence. */
const oneGiveUp = { giveUp: z.number().int().positive() };
const giveUpsThroughSeq = {
  consumer: nonEmptyString,
  throughSeq: z.number().int().positive(),
};

/** `api.IntakeReplayRequest`, the controller's replay of give-ups: its grant and one form of the
 * selection. Each form is a strict object, so a body that names both forms, or neither, parses as
 * neither. The daemon refuses such a body (`GIVE_UP_REQUEST_INVALID`), a grant that is not the
 * controller's (`CONTROLLER_REQUIRED`), an id of no give-up of its project (`GIVE_UP_NOT_FOUND`),
 * an id whose record a replay closed (`GIVE_UP_CLOSED`, naming how and when), an id a running
 * replay holds (`GIVE_UP_BEING_REPLAYED`), and every replay where it runs no intake
 * (`NO_INTAKE`). */
export const LegionIntakeReplayRequest = z.union([
  z.strictObject({ grantId: nonEmptyString, ...oneGiveUp }),
  z.strictObject({ grantId: nonEmptyString, ...giveUpsThroughSeq }),
]);

export type LegionIntakeReplayRequestBody = z.output<typeof LegionIntakeReplayRequest>;

/** `api.IntakeDiscardRequest`, the controller's discard of give-ups: the replay's body, and why its
 * records are let go. The daemon refuses what it refuses of a replay, an id whose record is closed
 * in any way included, and a reason that is blank or longer than 2,048 characters
 * (`GIVE_UP_REQUEST_INVALID`). */
export const LegionIntakeDiscardRequest = z.union([
  z.strictObject({ grantId: nonEmptyString, ...oneGiveUp, reason: nonEmptyString }),
  z.strictObject({ grantId: nonEmptyString, ...giveUpsThroughSeq, reason: nonEmptyString }),
]);

export type LegionIntakeDiscardRequestBody = z.output<typeof LegionIntakeDiscardRequest>;

/** `api.OperatorPauseRequest`, the operator's pause of the project (`legion project pause`, under
 * the operator's bearer): why the project is paused. The operator's resume sends no body, so it
 * has no schema here. The CLI sends both; the plugin never does. */
export const LegionOperatorProjectPauseRequest = z.strictObject({
  reason: nonEmptyString,
});

/** `api.OperatorP0BoundRequest` and `api.OperatorP0BoundClearRequest`, the operator's set and
 * clear of the P0 bound (`legion project set-p0-bound` and `clear-p0-bound`, under the operator's
 * bearer, the P0 start's contract entry). The CLI sends both; the plugin never does. */
export const LegionOperatorProjectP0BoundRequest = z.union([
  z.strictObject({ bound: LegionP0BoundNumber, reason: nonEmptyString }),
  z.strictObject({ unbounded: z.literal(true), reason: nonEmptyString }),
]);
export const LegionOperatorProjectP0BoundClearRequest = z.strictObject({
  reason: nonEmptyString,
});

/** `api.GateRegisterRequest`, the tree root's architect's current Dispatch document approval
 * target, for one step of the tree's design gate (`record.TreeSteps`); the daemon refuses it from
 * a child's own architect (`GATE_ROOT_ONLY`). `step` is the step the document is for; the
 * deployment's list says which steps a registration takes, with `criteria` beside a list that
 * names `spec` and not `criteria`. A deployment that gates one step alone takes a request that
 * leaves it out, and one that gates several refuses it (`GATE_STEP_REQUIRED`). `basisArtifactId`,
 * `basisVersion` and `reason` name a basis (entry 20261125161152 (gate-basis)): the same step's document of an ancestor
 * on the issue's Dispatch `child_of` chain, the version of it a person approved or an open
 * approval request asks a person to approve, and the architect's reason, 1 to 1,000 characters
 * with no decision block, which the daemon posts on the issue. The three come together or not at
 * all (`BASIS_INCOMPLETE`), and the daemon checks the basis against what Dispatch shows before it
 * records anything, refusing one it cannot take with a code that names what Dispatch shows
 * (`BASIS_*`, `ROOT_VERSION_NOT_LATEST`, `ROOT_CHANGES_REQUESTED`, `VIEW_*`, `REASON_INVALID`,
 * `REASON_HOLDS_DECISION_BLOCK`). */
export const LegionGateRegisterRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  artifactId: z.uuid(),
  version: z.number().int().positive(),
  step: legionDesignStep.optional(),
  basisArtifactId: z.uuid().optional(),
  basisVersion: z.number().int().positive().optional(),
  reason: nonEmptyString.optional(),
});

/** `api.WaveReleaseRequest`, the architect's selected child issues. */
export const LegionWaveReleaseRequest = z.strictObject({
  grantId: nonEmptyString,
  issues: z.array(nonEmptyString),
});

/** `api.PhaseBackwardRequest`, a request to return an issue to an earlier phase: a phase worker's
 * on its own issue, or an architect's on an issue it owns, from `accepting`. An architect may own
 * more issues than its own, so its request names `issue` (required for that grant); a phase
 * worker's grant names its own issue, so `issue` may be left out and, when given, must be the
 * grant's (`ISSUE_NOT_GRANTS`). Since entry 20261125161152 (gate-basis), the daemon refuses it on an issue in
 * `awaiting_children` (`WAITING_ON_CHILDREN`, naming its open Dispatch children and the start it
 * holds), which stands in no phase a move back acts on (dispatch://INFRA-3320, R11). */
export const LegionPhaseBackwardRequest = z.strictObject({
  grantId: nonEmptyString,
  to: z.enum(LEGION_PHASE_BACKWARD_TARGETS),
  reason: nonEmptyString,
  issue: nonEmptyString.optional(),
});

/** `api.PhaseRetryRequest`, the retry-or-escalate decision of the architect that owns a held issue.
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
 * its task to its next step, or to `retro` where the acceptance is the task's last step; the
 * daemon refuses it in every other phase (`SIGNOFF_OUTSIDE_ACCEPTING`). From
 * contract 219 the daemon judges it again when it applies it, and refuses it, changing nothing,
 * where the architect no longer owns the issue (`ISSUE_NOT_OWNED`, or `ISSUE_OUTSIDE_TREE` once
 * the issue left its tree), where the run of the issue it read has ended
 * (`SIGNOFF_STALE_GENERATION`), and where the run of `accepting` it read has ended
 * (`SIGNOFF_STALE_ENTRY`). Since
 * contract 51 it is also refused while an issue beneath that one is still in the workflow
 * (`SIGNOFF_CHILD_IN_WORKFLOW`, naming them): children are accepted and merged before their
 * parent, since a parent is accepted only once no issue beneath it is still in the workflow
 * (dispatch://INFRA-980), and it is accepted on a head that need not carry their merges once its
 * checks pass on a base commit that carries them (dispatch://INFRA-3320 R14). Since entry 20261125161152
 * (gate-basis) it is refused after that while a root whose design-gate step rests on the issue's approval is
 * still in the workflow (`SIGNOFF_RESTING_ROOT_IN_WORKFLOW`, naming each), and last while a
 * Dispatch child of the issue is open (`SIGNOFF_CHILD_OPEN`, naming each with its Dispatch
 * status): the daemon answers the first of the three that holds (dispatch://INFRA-3320, R8). On an
 * issue in `awaiting_children` it is refused `WAITING_ON_CHILDREN`, naming its open Dispatch
 * children and the start it holds, before any of those (R11). */
export const LegionSignOffRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
});

/** `api.RootCloseRequest`, a root architect's close of its tree before the tree's first phase starts,
 * with the reason the daemon posts on the issue. It is refused with the sign-off's three codes, in
 * the same order (`SIGNOFF_CHILD_IN_WORKFLOW`, `SIGNOFF_RESTING_ROOT_IN_WORKFLOW`,
 * `SIGNOFF_CHILD_OPEN`). */
export const LegionRootCloseRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  reason: nonEmptyString,
});

/** `api.ChildRequest`, an architect's rerun_child of one issue it owns: any child of its tree for
 * the tree root's architect, and only an issue under its own issue for a child's own architect
 * (`ISSUE_NOT_OWNED` otherwise). */
export const LegionChildRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
});

/** `api.PlanRegisterRequest`, register_plan (contract 219): the owner of an issue, an architect of
 * the issue's tree, gives Legion the plan in version `version` of the document
 * `artifactId` of the issue, which the daemon reads from Dispatch at that version. The issue may be
 * one the daemon has not recorded yet while Dispatch shows it triage, backlog, icebox or todo
 * (`ISSUE_ALREADY_RELEASED` otherwise). */
export const LegionPlanRegisterRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  artifactId: z.uuid(),
  version: z.number().int().positive(),
});

/** `api.TaskStopRequest`, stop_task (contract 219): the owner of a recorded issue stops one task of
 * the plan Legion runs for it, so that a later version of the plan may change that task; the issue
 * is then held with `holdReason` `task-stopped`. On an issue whose first step has not run, the
 * stop first settles the plan that step would walk, the newest plan its owner gave or the
 * project's default steps. A task the plan does not hold is refused (`TASK_NOT_FOUND`), and so is
 * one that is done (`TASK_DONE`), a stop in a tree that lingers after it left the workflow
 * (`TREE_LINGERING`), a stop of an issue outside the architect's tree or one that left it
 * (`ISSUE_OUTSIDE_TREE`), a stop of a run of the issue that ended before the daemon applied it
 * (`TASK_STALE_GENERATION`), a stop of a root that waits on a person before its first step
 * (`TASK_AWAITING_PERSON`), which a later version of the plan changes, or which stops once the
 * person has answered, every task of an issue whose experiment's group runs the project's
 * default steps, since its owner stops none of its tasks (`PLAN_NOT_TAKEN`, contract 220), and,
 * since entry 20261125161152 (gate-basis), a stop of a parent that waits in `awaiting_children` on its open Dispatch
 * children (`WAITING_ON_CHILDREN`, naming them and what the architect can do about each). A stop
 * on an issue that meets both is answered `PLAN_NOT_TAKEN`, since the daemon reads the issue's
 * group before anything of its plan. Each refusal writes nothing. */
export const LegionTaskStopRequest = z.strictObject({
  grantId: nonEmptyString,
  issue: nonEmptyString,
  task: nonEmptyString,
});
/** What plugin code takes the value of in one issue's record: `legion-phase-ops.ts` reads the
 * issue's phase for a fieldless `handoff_complete`; a named check report reads no record.
 * `phase-reminder.ts` and `pi-legion/src/stall-evidence.ts` read the architect's claim state.
 * The controller's `read_state` summary (`pi-legion/src/tools.ts`) reads each issue's status, hold
 * reason, slot and design gate, and the spec step's entry in its gated steps (its step, status and
 * basis). It also reads the state and both turn times of the architect's claim and every worker's
 * claim.
 * The record route and the state's `issues{}` answer the same node, so the read set below names
 * these paths at both, `LegionIssueRecordResponse` as they stand and `LegionStateResponse` under
 * the state's prefix. They are written once here because the two entries must say the same thing.
 *
 * The summary reads the state and never the record route. Its fields are in this list because
 * the summary reads them on the node that the two answers share. Each of them is `.optional()`
 * on `legionIssue`, so the record route refuses no body that does not carry one. A change that
 * makes one of them required for the state makes it required on the record route too. */
const ISSUE_RECORD_READS = [
  "phase",
  "status",
  "holdReason",
  "architect",
  "architect.state",
  "architect.lastTurnStart",
  "architect.lastTurnEnd",
  "workers",
  "workers{}.claim",
  "workers{}.claim.state",
  "workers{}.claim.lastTurnStart",
  "workers{}.claim.lastTurnEnd",
  "designGate",
  "designGate.currentVersion",
  "designGate.approvedVersion",
  "gatedSteps",
  "gatedSteps[].step",
  "gatedSteps[].status",
  "gatedSteps[].basis",
  "gatedSteps[].basis.issue",
  "gatedSteps[].basis.version",
  "slot",
  "slot.index",
  "slot.admittedAt",
  "slot.lentTo",
] as const;

/**
 * The declared read set: every field of an answer schema that a read in plugin code names, by the
 * answer schema's exported name and the path of the field within it. A read names a field below
 * an answer's root by the field's name under its parent's name, whichever answer schema that read
 * took the value from, so such a field is named in every answer schema that has one of that name
 * under a parent of that name. A field at an answer's root has no parent: a read names it by its
 * bare name, and this set names it only where plugin code takes its value off that answer, as it
 * names `LegionHealthResponse`'s `project` and not its `status`, or off the same node reached by
 * another path, since one node several paths reach is named at each of them (below): the issue
 * record's root is the state's `issues{}` node, so the record names `status`, `holdReason`,
 * `designGate`, `gatedSteps` and `slot`, which the `read_state` summary reads off the state's
 * issues.
 * A path names each key from that schema's root, with a record's value written `{}` and a list's
 * element `[]`, as the object census in `legion-api.test.ts` writes them, and a discriminated
 * union's member by its index after `|` (`paused.by|0.session`, the controller's entry). An
 * answer schema with no entry here is one whose fields no read in plugin code names.
 *
 * It is the other half of "the reader requires only what it reads" (dispatch://INFRA-2694). A
 * field this set does not name is `.optional()` above, so a daemon that serves this contract
 * without that field is still read; a field it names may stay required. The census in
 * `packages/pi-shared/src/daemon-client.test.ts` holds the two to each other in three
 * directions: a required field this set does not name fails it, a name of this set that no read
 * in a consumer source names fails it too, and a field a consumer source reads of its
 * own parent that this set does not name fails it as well. The consumer sources are
 * `packages/pi-shared/src`, `packages/pi-legion/src`, `packages/pi-legion/extensions` and
 * `packages/claude-envoy/src`, their test files left out.
 *
 * The census reaches the answer schemas the client parses and no others, reading the parse sites
 * of `packages/pi-shared/src/daemon-client.ts` for the roster. The operator and controller
 * answers the Go CLI reads and the client parses nowhere are outside it, and they keep every
 * field the daemon always writes required.
 *
 * A value handed to an agent whole is no read. The controller's `read_state` summarises the
 * state rather than passing it on, so the fields that summary takes are reads and are named
 * here (`pi-legion/src/tools.ts`), while a record a filter answers is handed on as it came and
 * is no read. A field a reader takes and the daemon
 * leaves out where it has none — `resumes[].firstTurnAt` — is named here and stays optional:
 * this set says what the plugin reads, not what the daemon always writes.
 *
 * One node several paths reach is named at each of them: the claim view is read at an issue's
 * `architect` and sits under `workers{}.claim` as well, and the issue record is the same node as
 * the state's `issues{}`, so both carry the same lines. As the first paragraph says, a field is
 * named in every answer that has one of its name under a parent of its parent's name, read there
 * or not: the pause and resume answers name the actor's `kind` and a controller's `session` under
 * `by` and `endedBy`, which the summary reads of the state's pauses, though no plugin code reads
 * either answer. A discriminator is required whatever a reader takes, since a body without it
 * matches no member of the union: one no plugin code reads (`locator.runtime`, `waitingOn.kind`)
 * is in no entry, and one the summary reads (the pause actor's `kind`) is named at each member,
 * as any read is.
 */
export const LEGION_ANSWER_READS: Readonly<Record<string, readonly string[]>> = {
  // The three readers of an issue's record ask the record route for it (dispatch://INFRA-2694),
  // and the controller's `read_state` summary reads the project's admission line, its
  // capability report and each issue's own fields. `daemon` and `daemon.project` stood
  // here until the controller's boot check read the project from the health answer instead
  // (`pi-legion/src/controller-session.ts`), and both are optional above.
  LegionStateResponse: [
    "issues",
    ...ISSUE_RECORD_READS.map((field) => `issues{}.${field}`),
    "admission",
    "admission.blocked[].issue",
    "admission.blocked[].blockedBy",
    // The summary's `held` lines name each held issue, its reason and the issues that hold it.
    // Its `trees` line reads `admission.trees` off the summary's own `admission`, as it reads
    // `cap`, `active` and `waiting`, which the census pairs with no parent. The census reads
    // `held.issue` as a read of every `issue` under a `held`, so the open pause's held rows name
    // their `issue` too, though the summary only counts them.
    "admission.held[].issue",
    "admission.held[].reason",
    "admission.held[].heldBy",
    // The summary's `P0` line names the P0 holders, the bound in force and the live value with who
    // set it, when and why (the P0 start's contract entry). The summary reads `admission.p0Holders` and
    // `admission.p0Bound` through the `admission` it destructures, which the census cannot follow,
    // as it cannot the block's older fields, so neither is named; the bound's own fields are, read
    // through the `p0Bound` the summary binds.
    "admission.p0Bound.bound",
    "admission.p0Bound.live",
    "admission.p0Bound.live.bound",
    "admission.p0Bound.live.setBy",
    "admission.p0Bound.live.setBy|0.kind",
    "admission.p0Bound.live.setBy|1.kind",
    "admission.p0Bound.live.setBy|0.session",
    "admission.p0Bound.live.setAt",
    "admission.p0Bound.live.reason",
    "capabilities",
    "capabilities[].name",
    "capabilities[].status",
    "capabilities[].detail",
    "capabilities[].configLine",
    // The summary prints `legion state`'s line for each repository the live records name that
    // GitHub's last read did not name or a role App is not installed on (#4065), with the name it
    // is served under. The block and its entries stay optional: a reader that finds none prints no
    // such line.
    "repositories",
    "repositories.live",
    "repositories.live[].repo",
    "repositories.live[].servedAs",
    "repositories.live[].apps",
    "repositories.live[].apps.implement",
    "repositories.live[].apps.review",
    "repositories.live[].readError",
    // The summary's line on the wakes the pacing holds counts them and names the earliest
    // `dueAt`, and its line on the project's pause (contract 178) names the pause that stands,
    // with the roles whose stop it waits on and how many rows it holds, or the last pause with
    // when and by whom it was ended. `wakesWaiting`, `paused` and `lastPause` stay optional: the
    // daemon writes the two pauses only where there is one, and strips all three for a caller
    // below the contract that added each.
    "wakesWaiting",
    "wakesWaiting[].dueAt",
    "paused",
    "paused.id",
    "paused.since",
    "paused.by",
    "paused.by|0.kind",
    "paused.by|1.kind",
    "paused.by|0.session",
    "paused.reason",
    "paused.waitsOn",
    "paused.waitsOn[].issue",
    "paused.waitsOn[].role",
    "paused.held",
    "paused.held[].issue",
    "lastPause",
    "lastPause.id",
    "lastPause.since",
    "lastPause.by",
    "lastPause.by|0.kind",
    "lastPause.by|1.kind",
    "lastPause.by|0.session",
    "lastPause.reason",
    "lastPause.endedAt",
    "lastPause.endedBy",
    "lastPause.endedBy|0.kind",
    "lastPause.endedBy|1.kind",
    "lastPause.endedBy|0.session",
    // The summary's lines on the intake's open give-ups (contract 228) print each consumer's
    // name, open count and newest sequence. `intakeGiveUps` stays optional: the daemon strips it
    // for a caller below 228. The lag view of the state's `consumers` has a `name` too, and the
    // census matches a read by the field's name under its parent's, so the summary's read of a
    // give-up count's `name` is read as one of the lag view's as well, which is why this set
    // names `consumers[].name`; the lag view keeps the field optional all the same.
    "intakeGiveUps",
    "intakeGiveUps.consumers",
    "intakeGiveUps.consumers[].name",
    "intakeGiveUps.consumers[].open",
    "intakeGiveUps.consumers[].newestSeq",
    "consumers[].name",
  ],
  // The same record read of one issue, by the same readers.
  LegionIssueRecordResponse: ISSUE_RECORD_READS,
  // The controller's boot check compares the daemon's project with its own LEGION_PROJECT.
  LegionHealthResponse: ["project"],
  // `legion-claim-core.ts` records every field of the claim the registration issued.
  LegionRegisterResponse: [
    "claimToken",
    "tree",
    "issue",
    "role",
    "generation",
    "secret",
    "promptAgents",
  ],
  // `controller-session.ts` checks the controller's claim token, mints every controller grant
  // with the secret, and sends the generation on `claims/ready`.
  LegionControllerRegisterResponse: ["claimToken", "generation", "secret"],
  // Every write of the `legion` tool posts the grant id `mintClaimGrant` took.
  LegionGrantResponse: ["grantId"],
  // `daemon-client.ts` reads both fields of a refusal: the code decides whether a 502 is a
  // restarting daemon, and the sentence is what the role is shown.
  LegionErrorResponse: ["error", "code"],
  // `read_release_records` counts the window's rows and measures the time between them. A wake
  // a project pause held carries `pause` and names no release, so the report keeps it under its
  // `issue` and lets it end the releases of that issue (contract 178).
  LegionReleaseRecordsResponse: [
    "releases",
    "releases[].id",
    "releases[].issue",
    "releases[].roles",
    "releases[].at",
    "resumes",
    "resumes[].release",
    "resumes[].issue",
    "resumes[].at",
    "resumes[].kind",
    "resumes[].firstTurnAt",
    "resumes[].pause",
    "holds",
    "holds[].reason",
  ],
  // No plugin code reads the release answer: the tool hands it to its agent whole. Its `held`
  // rows name their `issue` all the same, because the census reads the summary's `held.issue` of
  // the state's admission as a read of this as well (above).
  LegionWaveReleaseResponse: ["held[].issue"],
  // No plugin code reads the pause and resume answers: the controller hands each to its agent
  // whole. Each names the actor's `kind` and a controller's `session` under `by` and `endedBy`
  // all the same, because the census reads the summary's `by.kind`, `by.session`, `endedBy.kind`
  // and `endedBy.session` of the state's pauses as reads of these as well (above), and the actor
  // view is one node, required alike.
  LegionProjectPauseResponse: [
    "pause.by|0.kind",
    "pause.by|1.kind",
    "pause.by|0.session",
    "pause.endedBy|0.kind",
    "pause.endedBy|1.kind",
    "pause.endedBy|0.session",
  ],
  // No plugin code reads the P0 bound's answer: the controller hands it to its agent whole. Its
  // live value's fields are named all the same, because the census matches a read by the field's
  // name under its parent, and reads the summary's `live.bound`, `live.setBy.kind` and the rest of
  // the state's bound as reads of this answer's too.
  LegionP0BoundResponse: [
    "bound",
    "live.bound",
    "live.setBy",
    "live.setBy|0.kind",
    "live.setBy|1.kind",
    "live.setBy|0.session",
    "live.setAt",
    "live.reason",
  ],
  LegionProjectResumeResponse: [
    "pause.by|0.kind",
    "pause.by|1.kind",
    "pause.by|0.session",
    "pause.endedBy|0.kind",
    "pause.endedBy|1.kind",
    "pause.endedBy|0.session",
  ],
  // `pi-legion/src/turn-outcome.ts` logs the run of refused turns and whether this report
  // failed the claim on every turn it reports.
  LegionTurnOutcomeResponse: ["refusedRun", "failed"],
};
