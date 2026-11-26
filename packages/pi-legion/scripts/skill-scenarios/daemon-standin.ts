#!/usr/bin/env bun
/**
 * A stand-in for the Legion daemon that serves exactly the routes a phase worker's `legion` tool
 * and plugin hit in a skill scenario, of those the run's label's daemon registers (`ROUTES`): the
 * worker's claim registration (`claims/register`, `claims/ready`, the daemon's claim routes the
 * plugin boots through), the session's activity report (`claims/activity`, which the plugin posts
 * after each turn and the daemon answers 204 once the claim's secret authenticates it, as
 * `claims/ready` is: `s.activity` in packages/daemon/internal/api/claims.go), the outcome of each
 * turn (`claims/turn-outcome`, which this checkout's plugin posts after each turn: `turnOutcome`
 * there, answered with the run of refused turns, `TURN_OUTCOMES`), the idle-release records
 * (`claims/release-records`, the tool's `read_release_records`: `releaseRecords` there, answered
 * with none, since no pod of the rig is ever released), the grant the tool mints in-process before
 * each of its own daemon calls (`grants`), the issue's record (`GET /legion/v1/issues/<key>/record`,
 * the tool's `read_record` and the reminder's own check, with `GET /legion/v1/state` beside it),
 * the health route (`GET /healthz`, which the tool's `read_build` reads for the contract this
 * stand-in states on it), the phase completion (`handoff/complete`, the tool's
 * `handoff_complete`) and the return of the issue to an earlier phase (`phase/backward`, the
 * tool's `request_backward_move`), the two routes here that redeem a grant. No credential route
 * and no grant file: a pane's `gh` and `git` read the rig's token from the gh files under its
 * `GH_CONFIG_DIR`, which `daemon-pane.go` writes, and no agent runs `legion` from bash. A route
 * the label's daemon does not register is answered 404 whatever this has a handler for, as that
 * daemon's catch-all answers it, in its body (`{"error":"no route"}`,
 * packages/daemon/internal/api/server.go's mux remainder); a route it registers and this does not
 * answer is answered 404 too, which `rig.sh health` reads as a world the daemon would not have
 * served (health.ts's `routes` rule).
 *
 * Every request and response body is the Go daemon's (`@legion/contracts/legion-api`): the
 * tool's grant request, its completion, and the state document. The grant rule mirrors the real
 * daemon's: a grant lives for 60 seconds and redeems any number of times while it lives; an
 * unknown or expired grant id answers 403 `{"error":"Invalid or expired grant"}`. Nothing is
 * single-use. A completion is answered as the daemon of the run's label's checkout answers it
 * (`LATE_ROLE` and `RECORD_ROUTE`, read at boot out of the checkout the run's built-from names:
 * daemon-pane.ts's `labelDaemon`, never by the label's name), on the phase the issue stands in
 * now (`served()`), never on the phase the role was booted in. First the label's route
 * (`packages/daemon/internal/api/handoff.go`, completion-answers.ts's `RecordRoute`), which
 * decides whether the completion is record-backed. This checkout's shape (`left`): `of :=
 * phase.Of(grant.Role, issue.Phase); word, writer, handoffPhase := phase.Handoff(of);
 * recordBacked := handoffPhase && writer == grant.Role && (of == issue.Phase || namesRecord)`.
 * An older daemon's (`issue`, main `ff2a10ef`'s among them): `phase.Handoff(issue.Phase)` and
 * `recordBacked := handoffPhase && writer == grant.Role`. A record-backed completion naming no
 * record (`handoffId` and a positive `handoffVersion`) is refused 400 HANDOFF_RECORD_REQUIRED,
 * and one naming a record is read back from the run's own Dispatch (`handoffRecord`, as
 * handoff.go's `handoffRecord` reads it: `GET /api/v1/handoffs/{id}`) and refused where that
 * daemon refuses it: 404 HANDOFF_RECORD_NOT_FOUND for a record Dispatch has not got, 409
 * HANDOFF_RECORD_MISMATCH for one of another issue, phase or version, 502 DISPATCH_READ_FAILED
 * where Dispatch does not answer, and 500 DISPATCH_UNAVAILABLE for a run that names no Dispatch
 * (DISPATCH_URL), and 403 HANDOFF_SESSION_MISMATCH for one another session than the claim's
 * wrote, the claim's being the one its grant was minted for (`handoffRecord` says what was
 * measured: every record an agent of round 25 wrote names the session that agent registered).
 * This took every named record as found, a success production does not give a completion
 * naming a record that is not there; a
 * completion naming one of a phase that ends with none is refused 409 HANDOFF_RECORD_MISMATCH.
 * Then the fact: a completion is one fact by its role, the phase the issue stands in, the
 * implementer's pass, what it reported and its verdict (handoff.go's `eventID`), marked
 * processed before the workflow reads it, so a call repeating one the workflow was handed,
 * taken or refused, is refused 409 HANDOFF_ALREADY_RECORDED (`FACTS`), the completions the
 * world's seeds stand for among them.
 * Then the workflow (`packages/daemon/internal/workflow/engine.go`, `handoff`): a completion from
 * the role whose phase the issue stands in is taken `{}` and moves nothing, since the record
 * moves on the review, as main's engine moves it (`servedPhase`). A completion from any other
 * role (a late role) is answered by the label's engine (completion-answers.ts's `LateRole`):
 *   - this checkout's `leftItsPhase` (`recorded`) takes it, `{}` and moving nothing, when the
 *     daemon started the role on the issue and the role's row holds no completion
 *     (`row.Completed()`). The completion is then one of the phase the role left (`phase.Of`),
 *     whose record it must name, or it is refused 409 HANDOFF_RECORD_REQUIRED
 *     (`phase.Handoff(reported)`, through `refused`). A role whose row holds a completion is
 *     refused 409 HANDOFF_NOT_CURRENT_PHASE. That holds for each role the world's seeds record
 *     a round of (`HISTORY`: threads-closed's implementer and its reviewer, whose seeded
 *     records are on their rows), for the roles the rig says completed with a commit (the
 *     [completed] argument), and for a role whose completion was taken here, since the engine
 *     writes the completion's commit and record on the row (`row.HandoffCommit`,
 *     `row.HandoffRecord`; `completed`). It read the [completed] roles alone, and took a
 *     threads-closed reviewer's first late completion, which production refuses.
 *     The refusal of a role the daemon never started on the issue (`leftItsPhase`'s `RoleRun`)
 *     is not modelled. Unreachable: a completion reaches this stand-in only on a grant its
 *     booted agent's claim minted, the claim `claims/register` gave that agent for the role this
 *     stand-in was started in, and rig.sh's worker_boot starts a stand-in for each agent it boots.
 *     Main's engine since #4611 asks `leftItsPhase` through `pastCompletion` and `pastReportOf`
 *     (daemon-completion.go's `recordingChains`), which take the same late roles; their refusal
 *     of a late report of the run a stop of its task ended (workflow/plan.go's `lateReport`) is
 *     not modelled. Unreachable: a task stops only by its owner's `stop_task`, and no scenario
 *     boots an owner, or by a failed step no earlier step can take the work back from
 *     (`stopUnreturned`), and every failed step of the default steps has the implementer's before
 *     it.
 *   - an older engine (`refused`, main `ff2a10ef`'s among them) has no `leftItsPhase` and refuses
 *     every late completion 409 HANDOFF_NOT_CURRENT_PHASE (`if RoleFor(issue.Phase) !=
 *     fact.Role`).
 * A completion the engine takes that names the record its role's row last named is refused 409
 * HANDOFF_NOT_NEW (`LAST_HANDOFF`). Past it, where the label's workflow holds the refusal
 * (`RATING_REFUSED`, main's since #4488), a completion that rates the plan, its `planRating` with
 * its `planReason`, is refused 400 PLAN_RATING_UNEXPECTED in the engine's words (daemon-pane.ts's
 * `PLAN_RATING_UNEXPECTED`), naming the phase the completion is of, and its fact is left unspent
 * (`FACTS`), so the same completion sent again without the rating is taken: only the completion
 * that ends the retro rates the plan, and the `legion` tool sends a rating its agent gives on any
 * completion (legion-phase-ops.ts's `executePhaseOp`). The retro's own rating, PLAN_RATING_REQUIRED
 * for the completion that ends the retro with none and the rating's write onto the retro's record
 * (`PUT /api/v1/issues/{key}/steps/{id}/rating`: plan.go's `rateRetro`, outbox.go's
 * `publishStepRating`), is not modelled. Unreachable: no run of this rig stands in retro: rig.sh's
 * worker_boots starts each stand-in in `testing`, `reviewing` or `accepting`, every move of this
 * stand-in is the round's out of `reviewing` or a worker's back to an earlier phase (`enter`), and
 * the move out of `accepting` is the architect's sign-off, which no run boots. The route's
 * PLAN_RATING_INPUT, for a rating without its reason or off the rule of a rating (api's
 * `planRatingOf`), is not modelled: the contract this parses a completion with refuses such a
 * body 400 in zod's words. Unreachable: the `legion` tool refuses a lone `plan_rating` or
 * `plan_reason`, a rating off its scale and a reason blank or too long before it sends any
 * (legion-phase-ops.ts's `PhaseOpArguments`: its `planRating`, its `planReason` and the pair's
 * refines, through the contract's `legionPlanRatingScale`, `legionPlanReasonRules` and
 * `legionPlanRatingLoneField`). The plan record's write (`PUT /api/v1/issues/{key}/plans/{id}`:
 * plan.go's `WritePlan`, at the run's move out of `admitted` and at a hand-over's re-admission,
 * outbox.go's `publishPlanRecord`) is not modelled. Unreachable: every run begins past `admitted`
 * (rig.sh's worker_boots) and nothing re-admits its issue, and the one read of a plan, Dispatch's
 * `GET /api/v1/projects/{key}/plans`, lists a project's finished issues alone, which the world's
 * issue never is: no move of this stand-in takes it past `accepting`.
 * HANDOFF_SESSION_MISMATCH, HANDOFF_ALREADY_RECORDED and
 * HANDOFF_NOT_NEW are every daemon's the rig runs, and a label whose source names one nowhere is
 * refused before it boots (daemon-pane.ts's `labelShapes`). The route's rule on a completion's
 * verdict is every daemon's the rig runs too (`VERDICT_REFUSALS`, held to the label's source the
 * same way): a tester's verdict other than `pass` or `fail` is refused 400
 * TESTER_VERDICT_REQUIRED, and any other role's verdict 400 VERDICT_ROLE_FORBIDDEN, before the
 * fact. Its rules on an implementer's or a merger's commit (MISSING_FIELD) and on a `wakeAt`
 * (CHECK_WAKE_UNEXPECTED) are not modelled. Unreachable: the `legion` tool sends the commit the
 * workspace stands on, or the one its record names, with every implementer's completion
 * (pi-shared's `findHandoffReport`), no scenario boots a merger (rig.sh's worker_boots), and the
 * tool refuses a phase completion's `wake_at` before it sends one (legion-phase-ops.ts's
 * `completeHandoff`), a check report being the only completion that carries one, and the record
 * this serves holding no production check to report on.
 * The phase a completion is of (`phase.Of`) is the served phase when the role works it and
 * otherwise the role's own, its first (below): the implementer's would be retro once the issue
 * stands in merging, where no run of this rig stands. The route's entry fence (`entryFence`:
 * HANDOFF_STALE_ENTRY, or HANDOFF_NOT_CURRENT_PHASE in another role's phase, for a completion of
 * a phase entry its role's row has moved past, decided by `behindEntryClause` on the head the
 * record names) is not modelled: it compares two entries, the one the claim serves and the one
 * the role's row names, and those differ only once the issue has entered the role's phase again
 * while its worker was on the earlier entry. Unreachable: no run of this rig enters a phase
 * again. This stand-in's one forward move is the round's, out of `reviewing` (`servedPhase`);
 * every other move it makes (`enter`) goes back, a worker's to a phase before the one it works
 * (`backwardRefusal`), and no scenario boots a role after the reviewer (rig.sh's runners), so a
 * rig half's completion is always of the entry its row names.
 * The NOT_CURRENT_PHASE code and message are the daemon's own, read from its code at boot
 * (`NOT_CURRENT_PHASE`), because the skill has the worker quote them to its architect. The real
 * daemon's earlier fence, the 409 `HANDOFF_NO_RUN` of a completion it can attribute to
 * no run (`internal/api/handoff.go`, `noRunCompletion`), is not the one a rig half meets: both
 * halves are live registered workers serving generation 1, so the route finds a run and the
 * refusal is the workflow's. A role's phase is its first (daemon-pane.go's `phase <role>`),
 * which is narrower than the daemon, whose implementer also runs retro; no run of this rig
 * stands in retro. The body may carry every key any
 * label's contract sends (`CompleteRequest`). A backward move is the workflow's, as
 * engine.go's `backward` and `backwardRefusal` take it: it names no issue but this one
 * (`ISSUE_NOT_GRANTS`, 400, the daemon's refusal of a worker that moves another issue), and is
 * refused 409 BACKWARD_REFUSED, in the engine's own sentence, for a phase no backward row of the
 * issue's phase serves — any but an earlier one of the workflow's line (phase.go's `Order`) —
 * and for a role that does not run the issue's phase (phase.go's `RoleFor`). Then, where the
 * label's workflow holds the refusals (`REASON_REFUSALS`), the move's reason is held to the
 * message the move posts on the issue, its sentence (`backwardMoveReason`) read by Dispatch's
 * own reader as the daemon reads it (effects.go's `backwardReasonRefusal`, through the label's
 * own build: daemon-pane.ts's `postRefusal`), and refused 409 BACKWARD_REASON_DECISION_BLOCK for
 * a message holding a decision block and 409 BACKWARD_REASON_TOO_LONG for one past Dispatch's
 * bounds, each in the engine's sentence and moving nothing. Taken, it answers `{}` and moves the
 * issue to that phase, clearing the completion of the role the move starts there (engine.go's
 * `transition` and `clearHandoff`), so every later read of the record and every later completion
 * read the phase it moved to, and it posts its sentence on the run's Dispatch (`enter`); it
 * answered `{}` for any `to` and moved nothing, a success the daemon gives no move it refuses,
 * and then took a reason the daemon refuses and posted nothing. The tree's linger
 * (`TREE_LINGERING`) is not modelled. Unreachable: a tree lingers only once it has left the
 * workflow, done or closed, and no move of this stand-in reaches either (`enter`'s moves are the
 * round's and a worker's back), nor does any scenario boot the merger whose merge would (rig.sh's
 * worker_boots).
 *
 * The state document's record of the issue is production's (standin-record.ts, which says what
 * each member is): the tree's architect and a row for each role the world's history ran and
 * each agent the run booted, read back from the run's own Dispatch by the seeds <run
 * dir>/fixture.json names (`HISTORY`) and off the run's log of the stand-ins before this one;
 * this stand-in's own claim as its agent registers, readies and ends its turn (`OWN`); the pull
 * request as the world's gh answers it at the read (`worldPull`); the design gate as the run's
 * Dispatch holds the spec at the read (`gateNow`); and the slot, the entries and the counts of
 * the moves. Every move of the record — the round's, a backward move — is one `enter`: the phase,
 * the implementer's pass, pair and round, the start of the role that works the phase, and the
 * status that phase shows, written to the run's Dispatch as the daemon's own session before the
 * request is answered, after the move's reason where the move posts one (`runOutbox`). The pull
 * request carries `fixAttempts` where the label's daemon serves the count, main `ff2a10ef`'s
 * (`SHAPES.servesFixAttempts`), at 0, the count of every world (standin-record.ts's
 * `WORLD_FIX_ATTEMPTS`), and none where it serves none, main's since contract 59.
 *
 * A request body any string of which, a member's name or a value, holds U+0000 is refused 400
 * before any route reads it, where the label's daemon source holds that refusal (`NUL_REFUSED`,
 * main's since #4488), in that daemon's words, main's since #4463 or #4488's before them, naming
 * the first such string's place in the body and where the character stands in it (daemon-pane.ts's
 * `NUL_REFUSAL`, `nulRefusal` here): an agent's own text, a completion's summary or a move's
 * reason, can carry one, and the `legion` tool sends it as the agent gave it. #4463's store keeps
 * U+FFFD in the place of a U+0000 that reaches it, and no text of such a body reaches it, so its
 * answer differs from #4488's in the words alone. A byte that is not UTF-8 is read as U+FFFD, as
 * every daemon the rig runs decodes it, and taken as any other. Where the source holds no such
 * refusal, main `ff2a10ef`'s, whose decoder reads no string of a body, the body is taken to its
 * route, and the route answers it as that daemon's store does (daemon-pane.ts's `STORES_NUL`): a
 * completion the engine takes writes its summary and its commit into the role's row, a text
 * column, and a backward move it takes writes its reason into the task of the role it starts, a
 * jsonb payload; PostgreSQL refuses U+0000 in both, the fact's apply fails, and the route answers
 * 500 FACT_APPLY_FAILED in its own words, writing nothing and leaving the fact unspent
 * (`storeFails`). A completion or a move the engine refuses writes nothing and is answered its
 * refusal, and a ready keeps no character of the kind (capabilities' `cleanDetail`), so each is
 * taken as any other.
 *
 * Where the label's daemon source is main's since #3659 (dc1bbc995e: daemon-pane.ts's
 * `statesContract` and `retiresReady`, held to that source by `labelShapes`), every answer carries
 * `Legion-Daemon-Contract`, the daemon's own `DaemonAPIVersion` read off that source
 * (`DAEMON_CONTRACT`), a refusal, the catch-all's 404 and a 204 as much as a success; `GET
 * /healthz` answers the project beside its status (`HEALTH`); `GET
 * /legion/v1/issues/{key}/record` answers the issue's record as the state holds it, and 404
 * RECORD_NOT_FOUND in that daemon's words for any other key (`recordAnswer`); the state and the
 * record are the body that daemon serves a caller at its own contract, marked with that contract
 * as the daemon's and the caller's, the lowest contract its claims record, and each claim's update
 * state (`servedState`); and a completion sending `ready: true` is refused 400 READY_RETIRED once
 * its grant is redeemed (`RETIRES_READY`). A read the daemon would shape down to a contract below
 * its own is answered 500 in this stand-in's own words (`unshaped` says why no run reaches one). A
 * daemon before #3659, main `ff2a10ef`'s and the labels' before it, states no contract, answers
 * `GET /healthz` with `{"status":"ok"}` alone, registers no record route, so `ROUTES` answers it
 * 404, serves the state as this served it before, and has its completion's `ready` taken as this
 * took every label's (`CompleteRequest`).
 *
 * Where the label's daemon source is main's since #4493 (a86c31c9fd: daemon-pane.ts's
 * `repositoriesLive`, held to that source by `labelShapes`), the state's `repositories` block is
 * the repositories the project's live records name, the run's own world's one repository with both
 * role Apps installed on it, as a daemon that admitted the issue holds it (`REPOSITORIES_LIVE`); a
 * daemon before it serves the project's mapping, that one repository mapped as the default under
 * its owner, as this served every label before.
 *
 * The session holding each live role of the issue is published to the run's own Dispatch as the
 * daemon publishes it (`PUT /api/v1/issues/{key}/role-holders`: packages/daemon/internal/daemon/
 * outbox.go's `publishRoleHolders`, `queueHolders` here): at the stand-in's boot, at the agent's
 * registration, at its ready that carries no capability report, and at a turn outcome that fails
 * its claim, each the whole live set read when the write runs, so the run's Dispatch holds the
 * tree's architect, each role the world's history ran and each agent the run booted, as
 * production's does; it published nothing. A label whose daemon predates the publication, main
 * `ff2a10ef`'s, is published nothing (`PUBLISHES_HOLDERS`); both labels' checkouts, at main
 * `0b2769e5` or after, serve and publish it. A backward move's sentence is posted only where the
 * label's workflow posts one (`POSTS_BACKWARD`), in the words its source holds. Each of these is
 * read as one of two shapes of
 * the label's source, main's since the change that brought it or main `ff2a10ef`'s, which
 * predates it, and a source of neither refuses the batch, naming the marker and its file
 * (daemon-pane.ts's `labelShapes`), so none is answered as an older daemon for a marker gone
 * stale.
 *
 * What the daemon tells the roles of a head's CI is not modelled: the CI message naming each
 * check of the head with what it came to (packages/daemon/internal/workflow/review.go's
 * `tellChecks` and `settledChecks`), the task waking the worker of the open phase on a settlement
 * (wake.go's `wakeOnSettlement` and `settlementDetail`), and what main's daemon changed of both in
 * #4541 (6d50ef31): a check whose run GitHub skipped is named `skipped`, with the sentence that it
 * ran nothing on the head (`skippedClause`), a settlement of a head the record does not stand at
 * wakes nobody, and a settlement the daemon drops is named by an INFO line of its log
 * (`logDroppedSettlement`). Unreachable: no run's Envoy publishes a checks settlement, since
 * rig.sh's start_listener gives the label's listener no ENVOY_WEBHOOKS, so the listener mounts no
 * `/webhook/github` route and opens no CI store (packages/envoy/cmd/listener/main.go's
 * `webhookRoutes` and `ciStore`), whose summary loop is what publishes a settlement; and every
 * check a world's head reports is concluded `success` from the moment the head reaches the remote
 * (rig.sh's `concluded_checks`), so no world holds CI running on a head or a check GitHub skipped.
 *
 * Every request appends one JSON line to the log file: `{at, method, path, status, role, code?,
 * grantId?, sessionId?, mintedGrantId?, handoffId?, handoffVersion?, to?, reason?, phase?,
 * failed?}`, the run's
 * record of what reached the daemon, each line naming the role this stand-in serves, which is
 * what tells one half's completion from the other's in the one log a run keeps
 * (completion-answers.ts); a refusal's line carries its code where the answer has one, a
 * completion's line carries the record it named and the phase it was answered on (`served()`
 * then), and a backward move's the phase it
 * asked for and why (score.ts threadsClosed). The record opens with one line of its own,
 * `{at, opened}`, written when this starts listening, as record-publishes.ts writes its
 * `recording` line: a log holding that line alone is a run whose agent never reached the
 * daemon, which is a reading of the run, where a log that is not there at all is one nothing
 * ever wrote, which is a rig error (score.ts's `unscored`). No score reads the line itself.
 *
 * Usage: `bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <run dir>
 * <role> [phase] [completed]`. The daemon whose routes it answers is the one of the checkout
 * <run dir>/built-from names (`ROUTES`). The run's own Dispatch, which the world's history, the
 * spec's approval and a completion's handoff record are read back from and a move's status and
 * sentence and the issue's role holders are written to, is the one DISPATCH_URL names, reached
 * with the bearer in the file DISPATCH_TOKEN_FILE names (rig.sh's worker_boot gives both, the ones
 * the run's panes read). A backward move's reason is read as the label's daemon reads it by that
 * label's build of daemon-pane.go, the one SKILL_SCENARIOS_DAEMON_PANE names (worker_boot gives it),
 * or `go run` of it in the label's checkout where it names none.
 * The state document holds that one issue, on the repository and default branch of the run's own
 * world, which it reads from <run dir>/world.json, as the gh stand-in does: a record naming
 * another repository sends the agent's review to one the world has not got, and a record naming
 * another branch sends its work onto a branch the world has not got (rig.sh's worker world
 * comment). The issue stands in [phase], or,
 * where the caller names none, in the first phase its role works (daemon-pane.go's `phase
 * <role>`): the implementer works implementing and retro, and the daemon starts it in either.
 * [completed] is the roles, comma-separated, whose row the run's world gives a handoff commit:
 * each completed its phase before the run began (`COMPLETED`). A run directory holding a
 * fixture.json is a run with seeds: each record it names is read back from the run's Dispatch
 * at the boot, and a boot that cannot read one exits 2 naming it.
 * An issue in `reviewing` moves on what the same run directory reports: its remote's tip, the
 * reviews of its world and of its gh record, and the merge state its world reports for that tip
 * (`servedPhase`).
 * The secret a registration answers is drawn afresh at each boot (`SESSION_SECRET`), and is the
 * one SKILL_SCENARIOS_CLAIM_SECRET names where it names one: a caller that sends the secret
 * without registering first, as daemon-standin.test.ts's rows do. rig.sh names none.
 * Prints `listening on http://127.0.0.1:<port>` once the socket is open (port 0: the kernel's
 * pick).
 */
import { randomInt, randomUUID } from "node:crypto";
import { appendFileSync, existsSync, readdirSync, readFileSync } from "node:fs";
import { appendFile } from "node:fs/promises";
import { isLegionRole, roleToken } from "@legion/contracts";
import { daemonContract } from "@legion/contracts/daemon-contract";
import { STEP_OTHER_ISSUE_CODE, STEP_WRITER_CODE } from "@legion/contracts/dispatch-tools";
import {
  LegionGrantRequest,
  LegionHandoffCompleteRequest,
  LegionHealthResponse,
  LegionPhaseBackwardRequest,
  LegionReleaseRecordsRequest,
  LegionStateResponse,
  LegionTurnOutcomeRequest,
} from "@legion/contracts/legion-api";
import { z } from "zod";
import { COMPLETE_PATH } from "./completion-answers";
import {
  FACT_APPLY_FAILED,
  ISSUE_SLOT,
  labelCheckout,
  NUL_REFUSAL,
  PHASE_SLOT,
  PLAN_RATING_UNEXPECTED,
  type PostRefusal,
  postRefusal,
  READY_RETIRED,
  RECORD_NOT_FOUND,
  RECORD_PHASES,
  serves,
  standinFacts,
  VERDICT_REFUSALS,
} from "./daemon-pane";
import { worldOf } from "./gh-call";
import {
  DECIDES,
  heldPullRequest,
  mergeStateOf,
  recordedCalls,
  type SubmittedReview,
  submittedReviews,
  threadState,
} from "./gh-threads";
import { pusherOf } from "./rest-objects";
import { CALLS, Opened, openingLine } from "./run-records";
import {
  type CapabilityFacts,
  type GateFacts,
  GENERATION,
  type PullRequestFacts,
  phaseStatus,
  type RowFacts,
  stateBody,
  WORLD_FIX_ATTEMPTS,
} from "./standin-record";
import {
  answeredModel,
  endOf,
  runsStep,
  STEPS,
  type StepPhase,
  type StepRun,
  type StepWrite,
  stepEnd,
  stepReport,
  stepRun,
  stepStart,
} from "./step-records";

const GRANT_TTL_MS = 60_000;
const [
  portArg,
  logFile,
  bootTokenFile,
  project,
  issue,
  runDir,
  role,
  phaseArg = "",
  completedArg = "",
] = Bun.argv.slice(2);
if (
  portArg === undefined ||
  logFile === undefined ||
  bootTokenFile === undefined ||
  project === undefined ||
  issue === undefined ||
  runDir === undefined ||
  role === undefined
) {
  console.error(
    "usage: bun daemon-standin.ts <port> <log file> <boot token file> <project> <issue> <run dir> <role> [phase] [completed]"
  );
  process.exit(2);
}
if (!isLegionRole(role)) {
  console.error(`${role} is not a Legion role`);
  process.exit(2);
}
/** The roles the run's world says completed their phase with a commit before the run began
 * (rig.sh's worker_boot COMPLETED): each one's row holds that commit (`HANDOFFS`), and a row
 * that holds a completion is one a `recorded` engine refuses a late completion from
 * (`completed`). */
const COMPLETED = completedArg.split(",").filter((named) => named !== "");
for (const named of COMPLETED) {
  if (!isLegionRole(named)) {
    console.error(`daemon-standin: [completed] names ${named}, which is not a Legion role`);
    process.exit(2);
  }
}
/** The run's world, read through the one reader every program of the run reads it with
 * (gh-call.ts's `worldOf`): this stand-in answers a `read_record` out of it, the record's
 * `repo` and `defaultBranch` being the repository the world is on and the branch its pull
 * request is open against; and it reads the round's outcome off the issue branch of the run's
 * own remote, whose tip is the pull request's head, and the pull request's number, whose
 * reviews the world's routes answer. */
const world = worldOf(runDir, "daemon-standin");
const { repo, base, branch: BRANCH, pr: PR } = world;
/** The owner of the run's repository, which is the one owner the project maps. */
const [REPO_OWNER = ""] = repo.split("/");
if (REPO_OWNER === "" || !repo.includes("/")) {
  console.error(`${repo} is not an <owner>/<name> repository`);
  process.exit(2);
}
/** Encoded exactly as the daemon encodes it (`legion-<project>-<key>-<role>`, lower-cased),
 * since the worker claims this token on the real Envoy and Envoy rejects uppercase. */
const ROLE_TOKEN = roleToken(project, issue, role);
/** The session the daemon writes every Dispatch status of the issue under, and the author of a
 * handoff record it carried: `legion-daemon:<PROJECT>`, PROJECT being the key's
 * (packages/daemon/internal/dispatch/client.go's `DaemonSession`). */
const DAEMON_SESSION = `legion-daemon:${issue.split("-")[0]}`;
/** The letters of Go's `rand.Text()`: RFC 4648's base32 alphabet. */
const BASE32 = "ABCDEFGHIJKLMNOPQRSTUVWXYZ234567";
/** The secret a registration answers, in the shape the daemon mints one (Go's `rand.Text()`, 26
 * letters and digits of base32: packages/daemon/internal/api/claims.go), drawn afresh at each
 * boot, as the daemon's is random: the agent's plugin holds it and scrubs every run of 10 of its
 * characters from each line it prints (`@legion/contracts/held-secrets`), so a secret spelled
 * from words (it was `rig-secret`) took those words out of the lines a run reads. A caller that
 * sends it without registering first names it in SKILL_SCENARIOS_CLAIM_SECRET. */
const SESSION_SECRET =
  process.env.SKILL_SCENARIOS_CLAIM_SECRET ||
  Array.from({ length: 26 }, () => BASE32[randomInt(BASE32.length)]).join("");
/** A fact of the run's label's daemon read at boot, or the boot failed with why it could not be
 * read: a run whose daemon cannot be read is never answered as some other daemon would answer
 * it. */
function bootRead<T>(read: () => T): T {
  try {
    return read();
  } catch (error) {
    console.error(`daemon-standin: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(2);
  }
}
/** The checkout the run's label was built from (the run's built-from, daemon-pane.ts's
 * `labelCheckout`), whose daemon every fact below is read out of. */
const CHECKOUT = bootRead(() => labelCheckout(runDir));
/** Every fact of the run's label's daemon this answers by, read at boot by daemon-pane.ts's
 * `standinFacts`, which says what each is: its routes (`ROUTES`, the one answer to "does this
 * daemon serve that route", which `fetch` asks of every request), how it takes a completion
 * (`LATE_ROLE`, its engine's answer to a late role, and `RECORD_ROUTE`, the phase its route holds
 * a completion's record to, read out of the checkout's own source: completion-answers.ts's
 * `LabelDaemon`), its launch failure limit, its claim refusals, the phase the role's own worker is
 * started in and that phase's handoff word, the phase the issue stands in for this run (`PHASE`:
 * the caller's, or the role's own when it names none), and, for a run whose role can be late, the
 * daemon's own refusal of a late completion (`NOT_CURRENT_PHASE`), whose phase and issue are
 * filled in when it is sent. Each is read through daemon-pane.ts's facts cache, which rig.sh's
 * label_facts fills for every stand-in of a batch before any run starts, so a boot reads each out
 * of a file. A daemon whose completion shapes cannot be read fails the boot, rather than being
 * answered as some other daemon would answer it. */
const {
  routes: ROUTES,
  daemon: { lateRole: LATE_ROLE, recordRoute: RECORD_ROUTE },
  turnOutcomeLimit: TURN_OUTCOME_LIMIT,
  invalidSecret: INVALID_SECRET,
  invalidBootToken: INVALID_BOOT_TOKEN,
  staleGeneration: STALE_GENERATION,
  rolePhase: ROLE_PHASE,
  handoffWord: HANDOFF_WORD,
  phase: PHASE,
  notCurrentPhase: NOT_CURRENT_PHASE,
  shapes: SHAPES,
} = bootRead(() => standinFacts(CHECKOUT, role, phaseArg));
/** The label's workflow package, every source of it but its tests, which the sentences its moves
 * post on the issue and its refusals of a backward move's reason are read out of (`labelWords`). */
const WORKFLOW_SOURCE = bootRead(() => {
  const dir = `${CHECKOUT}/packages/daemon/internal/workflow`;
  return readdirSync(dir)
    .filter((name) => name.endsWith(".go") && !name.endsWith("_test.go"))
    .sort()
    .map((name) => readFileSync(`${dir}/${name}`, "utf8"))
    .join("\n");
});
/** Whether the label's workflow writes what its source shape says (`posts`, daemon-pane.ts's
 * `labelShapes`) in the words `literals` restate here, each a Go string literal of its source:
 * false for a workflow of the shape that writes none of it, and a failed boot, naming `what`, for
 * one that writes it and words some literal otherwise, which this would answer in another daemon's
 * words. */
function labelWords(posts: boolean, literals: readonly string[], what: string): boolean {
  if (!posts) return false;
  const unheld = literals.find((literal) => !WORKFLOW_SOURCE.includes(JSON.stringify(literal)));
  if (unheld !== undefined)
    throw new Error(
      `the workflow of ${CHECKOUT} ${what} and holds no ${JSON.stringify(unheld)}, so the words it writes are not the ones this stand-in answers in`
    );
  return true;
}
/** `format` with each verb (`%s`, `%d`) given the next of `values`, as Go's Sprintf fills them. */
function filled(format: string, ...values: readonly (string | number)[]): string {
  let at = 0;
  return format.replace(/%[sd]/g, () => String(values[at++] ?? ""));
}
/** The sentence a worker's backward move posts on the issue (effects.go's `backwardMoveReason`):
 * the phase it left, the one it went to, the role that asked, and that role's own words, or that
 * it gave none. */
const BACKWARD_ASKED = "Legion moved this issue from %s back to %s at the %s's request";
const BACKWARD_UNGIVEN = ", which gave no reason.";
/** Whether the label's daemon posts a worker's backward move on the issue at all (main's since
 * #4309, 26f27a997a). */
const POSTS_BACKWARD = bootRead(() =>
  labelWords(
    SHAPES.postsBackward,
    [BACKWARD_ASKED, BACKWARD_UNGIVEN],
    "posts a backward move's reason"
  )
);
/** Whether the label's workflow refuses a completion that rates the plan and does not end the
 * retro, PLAN_RATING_UNEXPECTED in the words daemon-pane.ts's `PLAN_RATING_UNEXPECTED` restates
 * (main's since #4488, held to the label's source by `labelShapes`). */
const RATING_REFUSED = SHAPES.refusesRating;
/** The words the label's daemon refuses a request body a string of which holds U+0000 with, a
 * format of daemon-pane.ts's `NUL_REFUSAL`: main's since #4463 or #4488's before them. Undefined
 * for a label whose decoder takes such a body, main `ff2a10ef`'s. Held to the label's source by
 * `labelShapes`. */
const NUL_REFUSED = SHAPES.nulRefusal;
/** Whether the label's store fails the apply of a taken request that writes `texts`, the agent's
 * own, where one holds U+0000: the store of a label whose decoder takes such a body, main
 * `ff2a10ef`'s (daemon-pane.ts's `STORES_NUL`, whose evidence it names), and no store of a label
 * that refuses the body before it. */
function storeFails(...texts: readonly (string | undefined)[]): boolean {
  return NUL_REFUSED === undefined && texts.some((text) => text?.includes("\0") === true);
}
/** The count of fix attempts the record's pull request carries: the world's, 0, where the label's
 * daemon serves the count (main `ff2a10ef`'s), and none where it serves none, main's since
 * contract 59 (daemon-pane.ts's `servesFixAttempts`, standin-record.ts's `WORLD_FIX_ATTEMPTS`). */
const FIX_ATTEMPTS = SHAPES.servesFixAttempts ? WORLD_FIX_ATTEMPTS : undefined;
/** The contract the label's daemon states on every answer and shapes its bodies to, its
 * `DaemonAPIVersion` read off its source (main's since #3659: daemon-pane.ts's `statesContract`),
 * or undefined for a daemon before it, whose answers state none and whose bodies are its own
 * whoever asks. */
const DAEMON_CONTRACT = SHAPES.statesContract;
/** Whether the label's daemon refuses a completion's `ready: true` (main's since #3659:
 * daemon-pane.ts's `READY_RETIRED`, held to the label's source by `labelShapes`). */
const RETIRES_READY = SHAPES.retiresReady;
/** Whether the state the label's daemon serves at its own contract carries the repositories its
 * live records name, `{live, withoutWebhookApp, caches}` (main's since #4493: daemon-pane.ts's
 * `repositoriesLive`, held to the label's source by `labelShapes`), or the project's mapping,
 * `{owners, mapped, refused, withoutWebhookApp, caches}`, as every daemon before it serves it
 * (standin-record.ts's `stateBody`). */
const REPOSITORIES_LIVE = SHAPES.repositoriesLive;
/** The engine's refusals of a backward move whose posted sentence Dispatch would refuse
 * (effects.go's `backwardReasonRefusal`): a decision block, and a message past Dispatch's bounds,
 * each filled with Dispatch's own sentence for it. */
const REASON_DECISION = {
  code: "BACKWARD_REASON_DECISION_BLOCK",
  format:
    "the reason holds a decision block, and the move posts the reason on the issue as a message, where nobody can answer one: %s; the move was not applied, so ask again with a reason that states what is wrong rather than asking a question",
} as const;
const REASON_TOO_LONG = {
  code: "BACKWARD_REASON_TOO_LONG",
  format:
    "the reason makes a message Dispatch refuses: %s; the move was not applied, so ask again with a shorter reason",
} as const;
/** Whether the label's workflow refuses a backward move's reason (main's since #4309). It reads
 * the sentence the move posts, so a workflow of that shape posts the sentence too: a source that
 * refuses and posts none is of no shape `labelShapes` reads. */
const REASON_REFUSALS = bootRead(() =>
  labelWords(
    SHAPES.refusesReason,
    [REASON_DECISION.code, REASON_DECISION.format, REASON_TOO_LONG.format],
    "refuses a backward move's reason"
  )
);
/** The answer of a claim refusal read at boot, in the daemon's shape (claim.Refusal marshals as
 * `{error}`). */
function refuse(refusal: { status: number; message: string }): Response {
  return json(refusal.status, { error: refusal.message });
}
/** The turn outcomes reported for this stand-in's one claim, by `<generation>:<sequence>`, each
 * whether the turn was refused: the daemon's window (supervise's `recordTurnOutcome`), where a
 * repeat of a key replaces it rather than adding one, kept in (generation, sequence) order and
 * trimmed to the newest `TURN_OUTCOME_LIMIT`. */
const TURN_OUTCOMES: { generation: number; sequence: number; refused: boolean }[] = [];
/** Whether the claim still holds the capability its registration's secret authenticates: until
 * a turn outcome fails the claim. The daemon fails it there (supervise's `fail`), and every write
 * of a failed claim clears its capability hash (`NormalizeClaim`, since `HoldsCapability` is
 * false for a failed claim), so each route that authenticates the secret (`authenticated` in
 * packages/daemon/internal/api/claims.go) answers it `InvalidSecret` from then on. */
let capabilityHeld = true;
/** Whether `secret` authenticates this claim, as the daemon's `authenticated` decides it. */
function authenticates(secret: string | undefined): boolean {
  return capabilityHeld && secret === SESSION_SECRET;
}

/** When this stand-in started: the daemon's boot in the state it serves, and the time of every
 * fact of the record that stood before any request reached it (`recordFacts`). */
const startedAt = new Date().toISOString();

/** The phase the stand-in serves now, which starts as the phase it was started for and moves as
 * main's engine moves it: `reviewRound` (`packages/daemon/internal/workflow/review.go`), and the
 * transitions its outcomes take (`workflow/table.go`). Only `reviewing` moves on its round: the
 * rig's run ends with any other phase's completion, and no scenario reads the record after it.
 *
 * A head that conflicts with its base is not modelled, nor is anything main's daemon does about
 * one since #4464, where the issue keeps its phase: the second read of the same head and base a
 * minute after a first conflicting one (review.go's `mergeability`, the `conflict_confirm` row of
 * daemon/conflict_confirm.go), the `merge_forward` message asking the implementer to merge the
 * base forward and the one telling the worker of the phase to stop writing the workspace
 * meanwhile (record's `MergeForwardAsk` and `MergeForwardHold`), the refusal of a completion at
 * the head a merge forward replaced (HANDOFF_HEAD_MERGED_FORWARD, merge_forward.go's
 * `mergedForwardRefusal`), the read of the merge state a push of the head or of the base asks for
 * (classify's `MergeStateReadOwed`), and the review it asks of the reviewer of an issue past its
 * review once the merge lands (`MergeForwardReview`); nor main `ff2a10ef`'s, which has no move of
 * its own on a conflict at all. A run reaches it by a forced push to the issue branch, which the
 * world's remote takes: its `proc-receive` hook (rig.sh's `fixture_first_commit`) checks no
 * fast-forward on any branch but the base, and on a scratch copy of run 94caf2e09844's remote a
 * forced push of another history answered `(forced update)`, exit 0, and `git merge-tree` of the
 * base and that head then refused to merge unrelated histories, exit 128. The world still reports
 * the head's merge state `CLEAN` or `BLOCKED` (gh-call.ts's `mergeStateOn`, gh-threads.ts's
 * `mergeStateOf`), and this stand-in moves on no conflict. A residual of the class search (the
 * architect's ruling of 2026-11-21T10:17:43Z, class-search.json): a run whose push leaves the
 * issue branch at a head that does not hold the base's tip is unhealthy from that push on
 * (health.ts's rule 24, `unmergedHead`), and an unhealthy run is replaced. The base itself never
 * moves during a run: the remote refuses every push to it and the world serves no merge.
 *
 * The review decides the round and the completion does not, so this moves on what the run's
 * world reports and never on a completion (`roundOutcome`, read at each state read): the head is
 * the tip of the run's own remote, the decision is the last `APPROVED` or `CHANGES_REQUESTED`
 * the engine takes among the reviews the world held when the run began and those the run left
 * (`takenForRound`), and GitHub's merge
 * state for the head is the one its base's rules leave it in with those reviews, the head's
 * checks and the run's review threads (gh-threads.ts's `mergeStateOf`, which the gh stand-in
 * answers `mergeStateStatus` with): a base that requires an approval holds it `BLOCKED` until one
 * stands, and one that requires conversation resolution until the last thread is resolved. It
 * does what `reviewRound` does with them:
 *   - a change request the engine takes ends the round rejected whether or not the reviewer has
 *     completed, and the issue goes back to `implementing` (`TriggerReviewRejected`): a person's
 *     whatever head it names, and the review App's only at the head its footer names, below.
 *     reviewer-text-findings' defect case reaches it, in `reviewing`;
 *   - an approval of the head (`classify.ApprovalStands`) ends the round approved when GitHub
 *     reports the head `CLEAN`, `UNSTABLE` or `HAS_HOOKS` (`record.MergeState.Mergeable`), and the
 *     issue moves to `accepting` (`TriggerReviewApproved`), on the approval alone
 *     (`review_test.go`'s "after the review alone"). Any other state — `BLOCKED`, which a check
 *     the base requires holds the head in while it runs, `BEHIND`, `UNKNOWN` — leaves the issue
 *     in `reviewing`, where the engine waits on GitHub's answer or reports the round stuck, and
 *     neither moves the record; a `DIRTY` one, a conflict, no world reports (above);
 *   - an approval of another head is set aside (`classify.ApprovalSetAside`, the round
 *     `stuckApprovedOtherCode`), which is what a push while the record is `reviewing` leaves, and
 *     the issue stays in `reviewing` until a review of the new head decides the round;
 *   - a review the review App submitted decides nothing where it is not the reviewer's own:
 *     main's engine since #4328 takes one only where its Legion footer names the session the
 *     reviewer's claim records, and waits while that claim records none; and since #2539 only
 *     where that footer names the pull request's head, 40 lowercase hexadecimal characters
 *     compared exactly, which is the head its decision stands on (`takenForRound`). One whose
 *     footer names no head, or another, leaves the issue in `reviewing`.
 * Once the record has moved on its round it stays where it moved to: a push in `accepting` lands
 * outside any round (`review.go`'s `issue.Phase != phase.Reviewing`, `review_test.go`'s "the
 * approval of the head, then a push"), and nothing the world reports moves it on. A reviewer's completion
 * after its decision is a late one, answered as the run's label's engine answers it (`handle`'s
 * `handoff/complete`).
 *
 * Not modelled: the `entry` a review's footer names, by which the reviewer's own deciding review
 * of the entry its row stands at moves the commit that review read (review.go's
 * `reviewedCommitStays`, `record.PhaseRow.ReviewedHead`), the commit the task of the reviewer's
 * next round names as `Last reviewed commit: <sha>` (review_round.go's `namingTheReviewRound`).
 * Unreachable: no scenario enters `reviewing` a second time, so none is given a later round's
 * task. This stand-in leaves `reviewing` at most once and never comes back to it (above), no
 * scenario boots an agent after its reviewer (rig.sh's runners), and the one reviewer task any
 * scenario gives is rig.sh's own, a first round's (`review-round "$co" '' 1` in
 * run_reviewer_text_findings: `Last reviewed commit: none. Review entry: 1.`), written before the
 * stand-in boots.
 *
 * The check runs of a head move nothing of their own, as they are none of `reviewRound`'s but for
 * the report of a stuck round (`classify.ChecksConcludedOnHead`): a check the base requires holds
 * the merge state `BLOCKED` until it passes, and the record moves on that state. Not modelled: a
 * push on its way (`classify.CodeOnItsWay`). Unreachable: the run's remote takes each push whole,
 * and the gh stand-in answers the pull request's head as that remote's tip at each call
 * (gh-standin.ts's `current`), the head this reads, so no head the world serves is one the pull
 * request is still waiting for. */
let servedPhase = PHASE;

/** The routes of the run's world and the merge state it reports for each head (rig.sh's
 * fixture_write), read afresh at each look, since the gh stand-in rewrites the file when a call
 * edits the pull request's body. A run with no fixtures.json has no world to read and no round
 * that ends. */
const Fixtures = z.looseObject({
  routes: z.array(
    z.looseObject({
      match: z.string(),
      methods: z.array(z.string()).optional(),
      stdout: z.unknown().optional(),
      graph: z.unknown().optional(),
    })
  ),
  mergeStates: z.unknown().optional(),
});
/** The merge states the issue may advance on (`record.MergeState.Mergeable`). */
const MERGEABLE = ["CLEAN", "UNSTABLE", "HAS_HOOKS"];

/** The run's pull request as its world says it now, read from the files the gh stand-in answers
 * it from, so the record and gh say one thing: the head its branch is at on the run's remote,
 * the state and the merge state GitHub reports for that head (gh-threads.ts's `heldPullRequest`
 * and `mergeStateOf`), and the review that last decided the round, the last whose state decides
 * one (gh-threads.ts's DECIDES, `classify`'s `DecidesRound`: a COMMENT and a pending review
 * decide nothing, and leave the decision before them standing) among the reviews the world held
 * and those the run left (gh-threads.ts's `submittedReviews`) that the engine takes
 * (`takenForRound`), with the head it is recorded against. That is the round's decision, which
 * main's record holds (`api.PullRequestView`'s `reviewDecision`, "the latest review round's
 * decision"), and not GitHub's review decision, which counts each reviewer's approvals against
 * the base's rule (gh-threads.ts's `reviewDecisionOf`). `round` is whether the world holds a round
 * to read at all: a run with no remote or no fixtures.json has none, and its pull request is the
 * one its world.json names, open, with no merge state read. */
function worldPull(): { pull: PullRequestFacts; round: boolean } {
  const tip = Bun.spawnSync([
    "git",
    "-C",
    `${runDir}/remote.git`,
    "rev-parse",
    `refs/heads/${BRANCH}`,
  ]);
  const head = tip.exitCode === 0 ? tip.stdout.toString().trim() : world.head;
  if (tip.exitCode !== 0 || !existsSync(`${runDir}/fixtures.json`))
    return { pull: { number: PR, head, state: "OPEN" }, round: false };
  const { routes, mergeStates } = Fixtures.parse(
    JSON.parse(readFileSync(`${runDir}/fixtures.json`, "utf8"))
  );
  const record = `${runDir}/${CALLS}`;
  const calls = existsSync(record) ? recordedCalls(record) : [];
  const at = { routes, world };
  let decision: PullRequestFacts["decision"];
  for (const [index, review] of submittedReviews(at, calls).entries()) {
    if (!DECIDES.includes(review.state)) continue;
    const taken = TAKEN.get(index) ?? takenForRound(review, head);
    if (taken === undefined) continue;
    TAKEN.set(index, taken);
    decision = { state: review.state, head: taken };
  }
  const threads = threadState(at, calls);
  const held = heldPullRequest(routes).state;
  return {
    pull: {
      number: PR,
      head,
      state: typeof held === "string" ? held : "OPEN",
      mergeState: mergeStateOf(at, threads, mergeStates, head, calls, () => pusherOf(runDir, head)),
      decision,
    },
    round: true,
  };
}

/** Each review the engine has taken, by its place among the pull request's reviews
 * (gh-threads.ts's `submittedReviews`, which lists the world's own and then the run's in the order
 * the run made them, so a review keeps its place), with the head its decision is recorded against.
 * The engine judges a review once, at the head the record holds when its webhook delivers it, and
 * keeps the decision it records (`record.ReviewDecision`) whatever the head comes to: an approval
 * a later push leaves on another head is set aside, not undone. So a review is judged at the
 * first read that finds it, the stand-in's delivery of it, and once taken stays taken; one the
 * engine refuses is judged again at the next read, as the daemon's read of the pull request's
 * reviews judges a review refused at the head the record held, or while the reviewer's claim
 * recorded no session, again (`RefusedAtThisHead`, `RefusedWhileNoSession`). Not modelled:
 * production's order of a review's webhook and its push's, in whose place the world reads the
 * remote's tip live (`worldPull`): production judges a review App review at the head the record
 * holds when its webhook is applied, so one whose webhook it applies before the push's is refused
 * there (`RefusedAtThisHead`) and taken at the per-issue read at the reviewer's handoff
 * (issue_reads.go's `readHandedOff`, review_reads.go's `reviewToApply`), where the world takes it
 * at the review. A residual of the class search (the architect's ruling of 2026-11-19T09:42Z):
 * the verdict is the same, taken earlier, and k1 scores the verdict. */
const TAKEN = new Map<number, string>();

/** The head the label's engine records `review`'s decision against, where it takes `review` for
 * the round at a read of the pull request at `head`, and undefined where it takes none
 * (review.go's `RefusesRound`, `reviewHead`). A review the world held before the run is the
 * history the world says the daemon took, and a review of any other App's pane is its writer's,
 * a person's, which decides as one; each stands on its `commit_id`, written whole where it names
 * the pull request's head by a prefix. A review the run made through a pane of the review App is:
 *   - the reviewer's own only where its Legion footer names the session the reviewer's claim
 *     records (`byReviewer`, `SHAPES.decidesBySession`): main's engine since #4328 reads that
 *     claim's session, which the reviewer's agent records at its registration (`register`), and
 *     takes no such review while it records none, so one submitted before the registration waits
 *     (`RefusedWhileNoSession`); main `ff2a10ef`'s reads the session off the claim the reviewer's
 *     row names, which only the reviewer's taken completion writes there (`completed`), and lets
 *     the login decide alone until then;
 *   - taken only where the head its footer names is `head`, compared exactly, and recorded against
 *     that head (`refusesRound`, `SHAPES.decidesByFooterHead`, main's since #2539): a footer that
 *     names no head, a head of anything but 40 lowercase hexadecimal characters, or another head
 *     is refused at this head, and its `commit_id`, which GitHub moves with the pull request,
 *     decides nothing. Main `ff2a10ef`'s engine reads no head off the footer and records the
 *     review against its `commit_id`, as a person's. */
function takenForRound(review: SubmittedReview, head: string): string | undefined {
  const named = review.head.length >= 7 && head.startsWith(review.head) ? head : review.head;
  if (review.made?.app !== "review") return named;
  const footer = legionFooter(review.made.body);
  const recorded = CLAIMS.get("reviewer")?.session ?? "";
  const held = completed("reviewer") ? recorded : "";
  const byReviewer = SHAPES.decidesBySession
    ? recorded !== "" && footer.session === recorded
    : held === "" || footer.session === held;
  if (!byReviewer) return undefined;
  if (!SHAPES.decidesByFooterHead) return named;
  return footer.head === head ? footer.head : undefined;
}
/** The fields of a review's Legion footer the label's decoder reads as strings (intake's
 * `legionFooter`): main's since #2539 decodes `session` and `head`, beside an `entry` it keeps
 * raw, which refuses no footer; main `ff2a10ef`'s decodes `session` alone. */
const FOOTER_STRINGS = SHAPES.decidesByFooterHead ? ["session", "head"] : ["session"];
/** The session and the head the last Legion footer of a review's `body` names, as the label's
 * daemon reads them (intake's `lastLegionFooter`, `LegionSession` and `LegionHead`): the JSON
 * object between the last `<!-- legion:` and the `-->` after it, read into the fields its decoder
 * declares (`FOOTER_STRINGS`). Both are "" where the body has none, no `-->` closes it, or what it
 * holds is no JSON object whose declared fields are strings or null where it names them (Go's
 * decoder refuses the whole footer then). Keys are matched as Go matches a field, without case.
 * The head is the footer's text as it stands: `takenForRound` compares it with the remote's tip,
 * a whole commit in git's 40 lowercase hexadecimal characters, so a prefix, capitals or a branch
 * name it holds match no head, as intake's `fullCommitSHA` reads them as no commit at all. */
function legionFooter(body: string): { session: string; head: string } {
  const none = { session: "", head: "" };
  const marker = "<!-- legion:";
  const at = body.lastIndexOf(marker);
  if (at < 0) return none;
  const rest = body.slice(at + marker.length);
  const end = rest.indexOf("-->");
  if (end < 0) return none;
  let footer: unknown;
  try {
    footer = JSON.parse(rest.slice(0, end));
  } catch {
    return none;
  }
  if (footer === null) return none;
  if (typeof footer !== "object" || Array.isArray(footer)) return none;
  const read = { session: "", head: "" };
  for (const [key, value] of Object.entries(footer)) {
    const field = key.toLowerCase();
    if (field !== "session" && field !== "head") continue;
    if (!FOOTER_STRINGS.includes(field)) continue;
    if (value !== null && typeof value !== "string") return none;
    if (value !== null) read[field] = value;
  }
  return read;
}

/** The phase the review round moves the issue to on the pull request `pull` (`worldPull`), or
 * undefined while it stays in `reviewing` (`servedPhase`). */
function roundOutcome(pull: PullRequestFacts): "accepting" | "implementing" | undefined {
  if (pull.decision?.state === "CHANGES_REQUESTED") return "implementing";
  return pull.decision?.head === pull.head && MERGEABLE.includes(pull.mergeState ?? "")
    ? "accepting"
    : undefined;
}

/** `GET /healthz` as the label's daemon answers it: that it is serving, and, where it states its
 * contract (`DAEMON_CONTRACT`), the project it serves, the one word the controller's boot check
 * reads (server.go's `Health`, read through this checkout's own contract); a daemon before #3659
 * answers `{"status":"ok"}` alone (daemon-pane.ts's `HEALTH_UNSTATED`). */
const HEALTH =
  DAEMON_CONTRACT === undefined
    ? { status: "ok" }
    : LegionHealthResponse.parse({ status: "ok", project });

/** The response header the daemon states its own contract in, on every answer, a refusal, the
 * catch-all's 404 and a 204 as much as a success (server.go's `statingContract`): the tool's
 * `read_build` reads it off `GET /healthz` beside its plugin's own. Set to `DAEMON_CONTRACT`
 * where the label's daemon states one, and on no answer of a daemon that states none. */
const DAEMON_CONTRACT_HEADER = "Legion-Daemon-Contract";
/** The request header a caller states its plugin's contract in, which a daemon that shapes its
 * bodies places the caller by (shaping.go's `pluginContractHeader`, `statedContract`): a header
 * that is no positive whole number is read as none. */
const PLUGIN_CONTRACT_HEADER = "Legion-Plugin-Contract";

interface LogLine {
  readonly at: string;
  readonly method: string;
  readonly path: string;
  readonly status: number;
  readonly role: string;
  readonly code?: string;
  readonly grantId?: string;
  readonly sessionId?: string;
  readonly mintedGrantId?: string;
  readonly handoffId?: string;
  readonly handoffVersion?: number;
  readonly to?: string;
  readonly reason?: string;
  readonly phase?: string;
  readonly failed?: boolean;
}

const expectedBootToken = (await Bun.file(bootTokenFile).text()).trim();
if (expectedBootToken.length === 0) {
  console.error(`boot token file ${bootTokenFile} is empty`);
  process.exit(2);
}

/** The grants the tool minted, each with the session it was minted for: the claim's own, which
 * the record a completion names is held to (`handoffRecord`). */
const grants = new Map<string, { readonly expiresAt: number; readonly sessionId: string }>();

/** `POST /legion/v1/handoff/complete`'s body: every key any label's contract sends, not this
 * checkout's alone, for the reason the state body carries every settings key (standin-record.ts's
 * `stateBody`): a
 * label's agents load the plugin of their own checkout. The base label, then built from main
 * `ff2a10ef`, sent `ready`, a boolean its contract requires on every completion; #2539 took it
 * out of this checkout's request, a strict object, so a stand-in parsing with this checkout's
 * alone answered every base completion 400 with zod's `unrecognized_keys`. Measured on
 * 2026-11-12: all 20 base runs of k1 and k2 logged `handoff/complete` 400 and nothing else, and
 * each scored a world of its own (review 5887659447, finding 1). The object stays strict, so a
 * key no label sends is still refused, and the daemon of that label answered a completion
 * carrying `ready` 200, as this does. A key a later contract drops stays here for as long as a
 * label the rig builds still sends it. */
const CompleteRequest = LegionHandoffCompleteRequest.extend({ ready: z.boolean().optional() });

/** The reviewer's decision the record holds: the round's, read from the world while the issue
 * stands in `reviewing` and kept as it stood when the issue left it, since a review outside a
 * round decides none (`api.PullRequestView`: "from the review that decided it until the next
 * round opens"). A stand-in started outside `reviewing` holds the one the world held then. */
let decided: PullRequestFacts["decision"];
let decisionRead = false;

/** The pull request the record holds on `read` of the world, after moving the record where the
 * round sends it once a read finds the round over while it is `reviewing` (`roundOutcome`,
 * `enter`). */
function settled(read = worldPull()): PullRequestFacts {
  if (servedPhase === "reviewing" || !decisionRead) {
    decided = read.pull.decision;
    decisionRead = true;
  }
  const outcome = servedPhase === "reviewing" && read.round ? roundOutcome(read.pull) : undefined;
  if (outcome !== undefined) enter(outcome, outcome === "implementing" ? "review" : undefined);
  return { ...read.pull, decision: decided };
}

/** The phase a state read serves now: `servedPhase`, as the world's round leaves it (`settled`). */
function served(): string {
  settled();
  return servedPhase;
}

function json(status: number, body: unknown): Response {
  return Response.json(body, { status });
}

function forbidden(message: string): Response {
  return json(403, { error: message });
}

/** A workflow refusal in the daemon's `Failure` shape, `{code, error}` (`writeFailure`). */
function failure(status: number, code: string, message: string): Response {
  return json(status, { code, error: message });
}

function stringField(body: unknown, key: string): string | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const value = (body as Record<string, unknown>)[key];
  return typeof value === "string" ? value : undefined;
}

function booleanField(body: unknown, key: string): boolean | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const value = (body as Record<string, unknown>)[key];
  return typeof value === "boolean" ? value : undefined;
}

function numberField(body: unknown, key: string): number | undefined {
  if (typeof body !== "object" || body === null) return undefined;
  const value = (body as Record<string, unknown>)[key];
  return typeof value === "number" ? value : undefined;
}

/** `{<key>: <value>}` for a value the request carried, and no key at all for one it did not: an
 * absent key is how a line says the body named none, which is what the log's readers take it for
 * (score.ts's DaemonLine). */
function carried<K extends string, V>(key: K, value: V | undefined): Record<K, V> | undefined {
  return value === undefined ? undefined : ({ [key]: value } as Record<K, V>);
}

/** Mirrors the real daemon's rule: present and not yet expired, nothing else. Each route parses
 * its body with its own contract schema first, exactly as the daemon's route table does, and
 * hands the parsed id here. */
function resolveGrant(grantId: string): Response | undefined {
  const grant = grants.get(grantId);
  if (!grant || grant.expiresAt <= Date.now()) return forbidden("Invalid or expired grant");
  return undefined;
}

/** The workflow's line of working phases, in the order it advances them (phase.go's `Order`):
 * the only phases a move back is read against, each with a backward row to every phase before
 * it (workflow/table.go's `backwardRows`, from phase.go's `BackwardPairs`). */
const WORKFLOW_ORDER = [
  "planning",
  "implementing",
  "testing",
  "reviewing",
  "accepting",
  "retro",
  "merging",
];

/** The role that works `phase` (phase.go's `RoleFor`), "" for a phase no role works. */
function roleFor(phase: string): string {
  switch (phase) {
    case "planning":
      return "planner";
    case "implementing":
    case "retro":
      return "implementer";
    case "testing":
      return "tester";
    case "reviewing":
      return "reviewer";
    case "admitted":
    case "accepting":
      return "architect";
    case "merging":
      return "merger";
  }
  return "";
}

/** Why the workflow refuses this role's move of the issue standing in `phase` back to `to`, in
 * the engine's own sentence (engine.go's `backwardRefusal`), or undefined where it takes it: a
 * target no backward row of `phase` serves — anything but an earlier phase of WORKFLOW_ORDER —
 * and a role that does not run `phase`. The contract refuses a move naming no target before it
 * is sent, so the engine's first case is never reached here. Both labels' daemons of INFRA-764
 * round 25 (the checkouts its runs' built-from name) word it so. */
function backwardRefusal(phase: string, to: string): string | undefined {
  const at = WORKFLOW_ORDER.indexOf(phase);
  const back = WORKFLOW_ORDER.indexOf(to);
  if (back < 0 || at < 0 || back >= at)
    return `a backward move reaches an earlier phase, and no backward row of ${phase} serves ${to}; the ${role}'s move of ${issue} changed nothing`;
  if (roleFor(phase) !== role)
    return `a backward move is the role running the issue's phase: ${phase} of ${issue} is the ${roleFor(phase)}'s, so the ${role}'s move to ${to} changed nothing`;
  return undefined;
}

/** `text` with the white space Go's `strings.TrimSpace` trims (`unicode.IsSpace`) cut from both
 * ends, which is not JavaScript's `trim`: Go trims U+0085 and keeps U+FEFF, and JavaScript the
 * other way about. */
function goTrimSpace(text: string): string {
  return text.replace(
    /^[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+|[\t\n\v\f\r \u0085\u00a0\u1680\u2000-\u200a\u2028\u2029\u202f\u205f\u3000]+$/g,
    ""
  );
}

/** What this role's backward move of the issue from `from` to `to` posts on the issue, as
 * effects.go's `backwardMoveReason` writes it (`BACKWARD_ASKED`): the role that asked is the role
 * running `from`, which the engine's guard holds the requester to, then its words, closed with a
 * full stop where they end with none of `.`, `!` and `?`. */
function backwardMoveReason(from: string, to: string, reason: string): string {
  const asked = filled(BACKWARD_ASKED, from, to, roleFor(from));
  const words = goTrimSpace(reason);
  if (words === "") return `${asked}${BACKWARD_UNGIVEN}`;
  return /[.!?]$/.test(words) ? `${asked}: ${words}` : `${asked}: ${words}.`;
}

/** The run's own Dispatch (rig.sh's worker_boot names it) and the file holding the bearer the
 * run reads it with, which a completion's handoff record is read back from (`handoffRecord`). */
const DISPATCH_URL = process.env.DISPATCH_URL ?? "";
const DISPATCH_TOKEN_FILE = process.env.DISPATCH_TOKEN_FILE ?? "";
/** A handoff record as Dispatch answers `GET /api/v1/handoffs/{id}`, as far as the daemon reads
 * it (dispatch/client.go's `handoffResponse`): what it is a record of, and who wrote it. */
const HandoffRecord = z.looseObject({
  issue_key: z.string(),
  phase: z.string(),
  version: z.number(),
  author: z.looseObject({ kind: z.string(), id: z.string() }).optional(),
});
/** A refusal Dispatch answers in its error body, `{code, error}` (dispatch/api's `errorBody`). */
const DispatchError = z.looseObject({ code: z.string().optional(), error: z.string().optional() });

/** An answer the run's own Dispatch refused: its status, the code its body carried ("" for none),
 * its message, and, as the error's own, what it answered. */
class DispatchRefusal extends Error {
  constructor(
    readonly status: number,
    readonly code: string,
    readonly refusal: string,
    answered: string
  ) {
    super(answered);
  }
  /** The refusal of `method route` Dispatch answered `status` with `text`, its error body
   * (`{code, error}`) read where it is one. */
  static of(method: string, route: string, status: number, text: string): DispatchRefusal {
    let refusal: z.infer<typeof DispatchError> = {};
    try {
      refusal = DispatchError.parse(JSON.parse(text));
    } catch {
      refusal = { error: text };
    }
    return new DispatchRefusal(
      status,
      refusal.code ?? "",
      refusal.error ?? "",
      `Dispatch answered ${method} ${route} ${status}: ${text}`
    );
  }
}

/** `GET route` of the run's own Dispatch as the run's agent bearer, its JSON answer. Throws a
 * DispatchRefusal for an answer Dispatch refused, naming the route and what Dispatch answered,
 * and an error for none at all or a run that names no Dispatch. */
async function dispatchRead(route: string): Promise<unknown> {
  if (DISPATCH_URL === "") throw new Error("the run names no Dispatch (DISPATCH_URL)");
  const token =
    DISPATCH_TOKEN_FILE === "" ? "" : (await Bun.file(DISPATCH_TOKEN_FILE).text()).trim();
  const answer = await fetch(`${DISPATCH_URL}${route}`, {
    headers: { Authorization: `Bearer ${token}` },
  });
  const text = await answer.text();
  if (!answer.ok) throw DispatchRefusal.of("GET", route, answer.status, text.trim());
  return JSON.parse(text);
}

/** Whether a Dispatch answer of `status` carrying `code` is one Dispatch gives the same write
 * every time (dispatch.PermanentRefusal): a 4xx carrying a code of Dispatch's own, but
 * ISSUE_CHANGED, a credential's 401 and 403, a 408, a 429 and a contended parent. */
function permanentRefusal(status: number, code: string): boolean {
  return (
    code !== "" &&
    code !== "ISSUE_CHANGED" &&
    status >= 400 &&
    status < 500 &&
    ![401, 403, 408, 429].includes(status) &&
    !(status === 409 && code === "PARENT_CONTENDED")
  );
}

/** `method route` of the run's own Dispatch with `body` as the daemon's own session writes it (its
 * `actor` as dispatch/client.go's `HTTPClient.actor` names it: the session of the key's project,
 * `DaemonSession`, or under dispatch_token_broker the daemon's own project's, the same on an issue
 * of that project). Throws a DispatchRefusal for an answer Dispatch refused, naming the route and
 * what Dispatch answered, and an error for none at all or a run that names no Dispatch. */
async function dispatchWrite(method: string, route: string, body: object): Promise<void> {
  if (DISPATCH_URL === "") throw new Error("the run names no Dispatch (DISPATCH_URL)");
  const token =
    DISPATCH_TOKEN_FILE === "" ? "" : (await Bun.file(DISPATCH_TOKEN_FILE).text()).trim();
  const answer = await fetch(`${DISPATCH_URL}${route}`, {
    method,
    headers: { Authorization: `Bearer ${token}`, "Content-Type": "application/json" },
    body: JSON.stringify({ ...body, actor: { kind: "session", id: DAEMON_SESSION } }),
  });
  if (!answer.ok)
    throw DispatchRefusal.of(method, route, answer.status, (await answer.text()).trim());
}

/** The run's seeds, <run>/fixture.json (seed-handoffs.ts's TesterFixture, which rig.sh's
 * worker_run_world and its runners write before any agent of the run boots): the handoff record
 * each phase before the one under test left on the run's issue, by its handoff word. A run with
 * none seeded no history, and its record holds the booted role's claim alone. */
const SEEDS = `${runDir}/fixture.json`;
const Seed = z.object({ id: z.string().min(1), version: z.number().int().positive() });
const Seeds = z.looseObject({
  plan: Seed.optional(),
  implement: Seed.optional(),
  test: Seed.optional(),
  review: Seed.optional(),
});
/** The roles a seed records a phase of, in the order the workflow runs them, each with the word
 * its record is seeded under, the phase its completion was of, the verdict it reported (a
 * tester's alone reports one, and the seeded tester's passed: seed-handoffs.ts records its
 * passing result and the status its pass moves the issue to), and whether it named a commit:
 * the implementer's names its code commit and the tester's the head it tested, each the head its
 * own seeded record names (the record's body's `head`: rig.sh's fixture_implement and
 * fixture_test_handoff write both at the implementer's commit, which the seeds record both results
 * at: worker_run_world and its runners), and never a later push of another role's: the reviewer's
 * text commit above it (rig.sh's approved_world) is no step of theirs, so their rows name the head
 * before it, as production's do. A planner's and a reviewer's name none, as production's rows of
 * those two carry none (production-shapes.json's `legion read_record`). */
const SEEDED = [
  { role: "planner", word: "plan", phase: "planning", verdict: "", commit: false },
  { role: "implementer", word: "implement", phase: "implementing", verdict: "", commit: true },
  { role: "tester", word: "test", phase: "testing", verdict: "pass", commit: true },
  { role: "reviewer", word: "review", phase: "reviewing", verdict: "", commit: false },
] as const;
/** An actor as Dispatch names one, the issue as it answers `GET /api/v1/issues/{key}`, a seeded
 * record with its author, and the spec's approval as `GET /api/v1/artifacts/{id}` carries it
 * (dispatch/model's `ArtifactApproval`), each as far as the record reads it. */
const Actor = z.looseObject({ kind: z.string(), id: z.string() });
const FiledIssue = z.looseObject({
  created_at: z.string(),
  created_by: Actor.nullable(),
  primary_artifact_id: z.string().min(1),
});
const SeededHandoff = HandoffRecord.extend({ author: Actor });
/** The body of a seeded record whose role names a commit (SEEDED's `commit`), as far as the row
 * reads it: the head the record names, as `dispatch handoff-write` stores the body, one JSON
 * object in a string (#5287's handoff shape). */
const SeededBody = z.object({ body: z.string() });
const SeededHead = z.looseObject({ head: z.string().regex(/^[0-9a-f]{40}$/) });
const Spec = z.looseObject({
  approval: z
    .looseObject({
      state: z.string(),
      latest_version: z.number(),
      version: z.number().optional(),
      reason: z.string().nullable().optional(),
    })
    .optional(),
});

/** What the run's world seeded before any agent of it booted, read back from the run's own
 * Dispatch so the record and Dispatch say one thing: when the issue was filed, which is when the
 * seeds hand it to Legion and it takes its slot (seed.ts's `testerProofIssue` files it and writes
 * the daemon's `in_progress` in one step); the session that filed it, the tree's architect
 * (seed.ts's FILER); its spec, the document the design gate is on; and each seeded record with
 * the session that wrote it, the role's own. A record the run's Dispatch holds as another
 * issue's, phase's or version's is a world the rig seeded wrongly, refused here. */
interface History {
  readonly admittedAt: string;
  readonly architect: string;
  readonly spec: string;
  readonly seeded: readonly {
    readonly role: string;
    readonly session: string;
    readonly record: { readonly id: string; readonly version: number };
    /** The commit the role's row names, where its role names one (SEEDED's `commit`). */
    readonly commit: string | undefined;
    readonly phase: string;
    readonly verdict: string;
  }[];
}
const seededHistory = async (): Promise<History> => {
  const seeds = Seeds.parse(JSON.parse(readFileSync(SEEDS, "utf8")));
  const filed = FiledIssue.parse(await dispatchRead(`/api/v1/issues/${encodeURIComponent(issue)}`));
  const seeded: History["seeded"][number][] = [];
  for (const { role: named, word, commit, phase, verdict } of SEEDED) {
    const record = seeds[word];
    if (record === undefined) continue;
    const answer: unknown = await dispatchRead(`/api/v1/handoffs/${encodeURIComponent(record.id)}`);
    const held = SeededHandoff.parse(answer);
    if (held.issue_key !== issue || held.phase !== word || held.version !== record.version)
      throw new Error(
        `${SEEDS} names ${record.id} as version ${record.version} of ${issue}'s ${word} handoff, which the run's Dispatch holds as version ${held.version} of ${held.issue_key}'s ${held.phase} handoff`
      );
    const head = commit
      ? SeededHead.parse(JSON.parse(SeededBody.parse(answer).body)).head
      : undefined;
    seeded.push({ role: named, session: held.author.id, record, commit: head, phase, verdict });
  }
  return {
    admittedAt: filed.created_at,
    architect: filed.created_by?.kind === "session" ? filed.created_by.id : "",
    spec: filed.primary_artifact_id,
    seeded,
  };
};
/** The run's history, or the boot failed with why it could not be read: a run whose seeds
 * cannot be read back is never answered with a record of some other history. */
let HISTORY: History | undefined;
if (existsSync(SEEDS)) {
  try {
    HISTORY = await seededHistory();
  } catch (error) {
    console.error(
      `daemon-standin: the history ${SEEDS} names could not be read back from the run's Dispatch: ${error instanceof Error ? error.message : String(error)}`
    );
    process.exit(2);
  }
}

/** A claim of the issue as this stand-in holds it (standin-record.ts's `ClaimFacts`). */
interface Claim {
  session: string;
  state: string;
  generation: number | undefined;
  capabilities: CapabilityFacts | undefined;
  served: boolean;
  pendingTask: { phase?: string; queuedAt: string } | undefined;
  lastTurnStart: string | undefined;
  lastTurnEnd: string | undefined;
  lastReady: string | undefined;
}
/** A claim in `state` under `session`, of the process `generation` names (none: no process). */
function claimOf(
  session: string,
  state: string,
  generation: number | undefined,
  served: boolean
): Claim {
  return {
    session,
    state,
    generation,
    capabilities: undefined,
    served,
    pendingTask: undefined,
    lastTurnStart: undefined,
    lastTurnEnd: undefined,
    lastReady: undefined,
  };
}
/** The processes the issue's pod has started so far: each claim's process is the next, as an
 * issue pod's launcher numbers them (`runtime.SandboxLocator.Generation`). */
let launches = 0;
function launched(): number {
  launches += 1;
  return launches;
}
/** The claims of the issue, by role (the architect's among them), and the handoff each role's
 * completion of its current phase named: what the record's `architect` and `workers` are built
 * from (`recordFacts`). */
const CLAIMS = new Map<string, Claim>();
const HANDOFFS = new Map<
  string,
  { commit: string | undefined; record: { id: string; version: number } | undefined }
>();
/** Whether `named`'s row holds a completion (`record.PhaseRow.Completed`): a handoff commit or a
 * handoff record, which a `recorded` engine refuses a late completion from (`leftItsPhase`). */
function completed(named: string): boolean {
  const held = HANDOFFS.get(named);
  return held?.commit !== undefined || held?.record !== undefined;
}
/** The record each role's row last named (`record.PhaseRow.LastHandoff`), which a move that
 * clears the row's handoff keeps, and the completions the workflow has been handed, each by the
 * key its route gives one (`factOf`). */
const LAST_HANDOFF = new Map<string, string>();
const FACTS = new Set<string>();
/** The key of one completion (handoff.go's `eventID`, less the issue and its generations, which
 * are one in a run): the role, the phase the issue stands in, the implementer's pass through
 * it, what the completion reported — the record it names where it is record-backed, else its
 * commit — and its verdict. */
function factOf(
  named: string,
  phase: string,
  passes: number,
  reported: string,
  verdict: string
): string {
  return [named, phase, passes, reported, verdict].join("\0");
}

/** A registration of `claim`'s agent under `session`: a claim that was registered before is
 * relaunched, a new process of the pod. */
function register(claim: Claim, session: string | undefined): void {
  if (claim.state !== "launching") claim.generation = launched();
  claim.session = session ?? claim.session;
  claim.state = "registered";
  claim.lastTurnStart = undefined;
}
/** `claim`'s agent saying at `at` that it can be prompted. A rig agent is `omp -p`, handed its
 * prompt at its launch, so the turn the daemon would record at the task's delivery
 * (supervise's `LastTurnStart`, `StateWorking`) begins where the agent first says so. */
function ready(claim: Claim, at: string): void {
  claim.lastReady = at;
  if (claim.state !== "registered") return;
  claim.state = "working";
  claim.lastTurnStart = at;
}
/** The end of a turn of `claim`'s agent at `at`, which retires the task the turn confirmed
 * (`servingGeneration`); a turn that failed the claim leaves it with no process. */
function endTurn(claim: Claim, at: string, failed: boolean): void {
  claim.lastTurnEnd = at;
  if (claim.pendingTask !== undefined) {
    claim.pendingTask = undefined;
    claim.served = true;
  }
  claim.state = failed ? "failed" : "idle";
  if (failed) claim.generation = undefined;
}

// The history first, in the order the workflow ran it: the architect, then each role whose
// record a seed wrote, each a claim whose turn ended before this daemon recorded it, so each is
// idle and carries no time of a turn (standin-record.ts's header). A seeded record stands for
// the completion that named it: it is on its role's row, it is the record that role last named,
// and it is a fact the workflow already took, of the role's own phase on the issue's first pass.
if (HISTORY !== undefined) {
  CLAIMS.set("architect", claimOf(HISTORY.architect, "idle", launched(), true));
  for (const seed of HISTORY.seeded) {
    CLAIMS.set(seed.role, claimOf(seed.session, "idle", launched(), true));
    HANDOFFS.set(seed.role, { commit: seed.commit, record: seed.record });
    LAST_HANDOFF.set(seed.role, seed.record.id);
    FACTS.add(factOf(seed.role, seed.phase, 0, seed.record.id, seed.verdict));
  }
}
// A role the rig says completed its phase with a commit before the run began (`COMPLETED`) holds
// that commit on its row, the head the run began at, where no seed gave the row its record too.
for (const named of COMPLETED)
  if (!HANDOFFS.has(named)) HANDOFFS.set(named, { commit: world.head, record: undefined });
/** One line of the run's record of what reached its daemon, as an earlier stand-in of the run
 * wrote it (`LogLine`). */
const Logged = z.looseObject({
  at: z.string(),
  path: z.string(),
  status: z.number(),
  role: z.string().optional(),
  sessionId: z.string().optional(),
  failed: z.boolean().optional(),
});
// Then each agent the run booted before this one, read off the run's one record of what reached
// its daemon, which every stand-in of the run appends to (rig.sh's worker_boot): the session it
// registered, when it said it could be prompted and when its turn ended. rig.sh boots an agent
// only after the one before it has exited, so each such claim is idle now, or failed where a
// turn outcome failed it. threads-closed's reviewer reads the implementer's claim so.
if (existsSync(logFile)) {
  const earlier = new Set<Claim>();
  for (const text of readFileSync(logFile, "utf8").split("\n")) {
    if (text.trim() === "") continue;
    const value: unknown = JSON.parse(text);
    const line = Opened.safeParse(value).success ? undefined : Logged.safeParse(value).data;
    if (line?.role === undefined) continue;
    if (line.path === "/legion/v1/claims/register" && line.status === 200) {
      const claim = CLAIMS.get(line.role) ?? claimOf("", "launching", launched(), false);
      register(claim, line.sessionId);
      CLAIMS.set(line.role, claim);
      earlier.add(claim);
    }
    const claim = CLAIMS.get(line.role);
    if (claim === undefined || !earlier.has(claim)) continue;
    if (line.path === "/legion/v1/claims/ready" && line.status === 204) ready(claim, line.at);
    if (line.path === "/legion/v1/claims/turn-outcome" && line.status === 200)
      endTurn(claim, line.at, line.failed === true);
  }
  for (const claim of earlier) if (claim.state !== "failed") claim.state = "idle";
}
/** This stand-in's own claim, the booted agent's: a new process of its role's claim, which the
 * daemon is launching until the agent registers. */
const OWN = claimOf(
  CLAIMS.get(role)?.session ?? "",
  "launching",
  launched(),
  CLAIMS.get(role)?.served ?? false
);
CLAIMS.set(role, OWN);

/** The entry each role stands on and the entry its claim was last given a task of
 * (`record.PhaseRow.Entry`, `supervise.Claim.GivenEntry`). A start raises its role's entry
 * (workflow's `enter`), and the task it queues is of that entry, so the two are equal for a role
 * that holds a claim and the second is 0 for one that holds none. */
const ENTRIES = new Map<string, { entry: number; given: number }>();
function raise(named: string): void {
  const held = ENTRIES.get(named) ?? { entry: 0, given: 0 };
  held.entry += 1;
  if (CLAIMS.has(named)) held.given = held.entry;
  ENTRIES.set(named, held);
}
/** The task a start of `phase` queues to its role at `at` (`api.PendingTaskView`): of that phase,
 * and of no phase for the architect's. */
function taskOf(phase: string, at: string): NonNullable<Claim["pendingTask"]> {
  return roleFor(phase) === "architect" ? { queuedAt: at } : { phase, queuedAt: at };
}
// The entries the history raised: the architect's at the issue's admission, each seeded role's
// at the start of its phase, and the start of the phase the issue stands in, where that is the
// first of its role or the architect's second, its acceptance. The role working that phase holds
// its task until a turn of its retires it, unless its row already holds the phase's completion:
// tester-proof's tester and reviewer-text-findings' reviewer hold the task their prompt is, and
// threads-closed's halves hold none, since the daemon starts no phase for that loop.
raise("architect");
for (const { role: named } of HISTORY?.seeded ?? []) raise(named);
if (roleFor(PHASE) !== "" && (PHASE === "accepting" || !ENTRIES.has(roleFor(PHASE))))
  raise(roleFor(PHASE));
const WORKING = CLAIMS.get(roleFor(PHASE));
if (WORKING !== undefined && !completed(roleFor(PHASE)))
  WORKING.pendingTask = taskOf(PHASE, startedAt);

/** The implementer's counts, which every move back raises (workflow's `recordPass`: the pass and
 * the move's own pair) and a review that requested changes raises once more (`recordRound`). */
let pass = 0;
let rounds = 0;
const backwardMoves: Record<string, number> = {};
/** When the issue took the slot it holds, and when the daemon last saw anything of its tree move:
 * a request of the claim's that it took, or a move of the record. No move of the rig brings an
 * issue back before its review once it is past it (a backward move is its phase's own role's,
 * and `accepting` is the architect's, which no run boots), so a slot given up is never taken
 * again and the time it was taken at never moves. */
const admittedAt = HISTORY?.admittedAt ?? startedAt;
let movedAt = startedAt;

/** The Dispatch status the record holds: the one its phase shows, and for a stand-in started in
 * a phase with none of its own the hand-over's `in_progress` (`phaseStatus`). */
let shownStatus = phaseStatus(PHASE) || "in_progress";
/** A row the record's moves and the claims' changes owe the run's Dispatch, as the daemon's
 * outbox holds one (`record.OutboxRow`), numbered as the outbox numbers its rows (a posted
 * message's marker names its row): a status a move gives the issue, with the sentence the move
 * posts before it where the move posts one (`record.StatusWrite`, a `dispatch_status` row); that
 * sentence alone, for a move into a phase that shows the status the issue already shows
 * (`record.MessagePost`, a `dispatch_message` row, as `Engine.status` queues it); the
 * publication of the issue's role holders (`record.RoleHoldersPublish`), one row for the issue,
 * which reads the live set when it runs (`queueHolders`); and one write of the record of a run of
 * a step (`record.StepRecord`, a `step_record` row: `queueStep`), the record's id and the write.
 * Each keeps when it was queued, which bounds the outbox's search for a message it posted
 * (`postMessage`), the attempts it made and why the last failed. */
type OutboxRow = {
  readonly id: number;
  readonly createdAt: number;
  attempts: number;
  lastError?: string;
} & (
  | {
      readonly kind: "status";
      readonly status: string;
      readonly observed: string;
      readonly reason: string;
    }
  | { readonly kind: "message"; readonly body: string }
  | { readonly kind: "holders" }
  | { readonly kind: "step"; readonly step: string; readonly write: StepWrite }
);
/** The rows the run's Dispatch is owed and has not taken, oldest first (`runOutbox`). */
const OUTBOX: OutboxRow[] = [];
let outboxRows = 0;
/** A row as its queue site gives it: its kind's fields, before the outbox numbers it. */
type Unqueued<Row> = Row extends unknown ? Omit<Row, "id" | "createdAt" | "attempts"> : never;
/** Queues `row` as the next row of the outbox, numbered after the last and queued now. */
function queueRow(row: Unqueued<OutboxRow>): void {
  outboxRows += 1;
  OUTBOX.push({ ...row, id: outboxRows, createdAt: Date.now(), attempts: 0 } as OutboxRow);
}
/** How many changes of a claim have asked for the role holders' publication: the row is
 * finished only by a write that began after the last of them, as the daemon's queue rotates the
 * lease of a row in flight so that it runs again with the newer set (store's
 * `EnqueueRoleHolders`). */
let holdersAsked = 0;
/** Whether the label's daemon publishes the issue's role holders at all, to a Dispatch that
 * serves the route it publishes to, the label's, the one the run's Dispatch is built from
 * (rig.sh's services_up): main's daemon and Dispatch since #4228 (5a2d1f0637), its client's
 * `PutRoleHolders` and the route's row in Dispatch's table (daemon-pane.ts's `labelShapes`). A
 * daemon of main `ff2a10ef`'s shape publishes nothing here. */
const PUBLISHES_HOLDERS = SHAPES.publishesHolders;
/** Queues the publication of the issue's role holders, where the daemon queues one: at its boot
 * (`queueRoleHoldersPublish`), at a claim write that changes the session holding a role — an
 * agent's registration, a claim the daemon fails (store's `putClaim`) — and at a ready that
 * carries no capability report (api's `ready`, `roleHoldersChanged`). The issue has one such row,
 * made due again by a second change. A run that names no Dispatch queues none, as a daemon with
 * no `dispatch_url` runs no outbox and queues no row (store's `PublishRoleHolders`), and neither
 * does a label that publishes none (`PUBLISHES_HOLDERS`). */
function queueHolders(): void {
  if (DISPATCH_URL === "" || !PUBLISHES_HOLDERS) return;
  holdersAsked += 1;
  if (!OUTBOX.some((row) => row.kind === "holders")) queueRow({ kind: "holders" });
}
/** The claim states in which a claim's session holds its role (`supervise.HoldsCapability`,
 * record's `liveClaimStates`): a registered agent with a running process. */
const LIVE_STATES = ["registered", "ready", "working", "idle"];
/** The session of each live claim of the issue, by role, in the order the daemon's read lists
 * them (record's `LiveRoleHolders`, `order by role`): the set a publication sends whole. */
function liveHolders(): { role: string; session: string }[] {
  return [...CLAIMS]
    .filter(([, claim]) => claim.session !== "" && LIVE_STATES.includes(claim.state))
    .map(([named, claim]) => ({ role: named, session: claim.session }))
    .sort((one, other) => (one.role < other.role ? -1 : one.role > other.role ? 1 : 0));
}

/** Whether the label's daemon records each run of a step of the issue on its Dispatch, and that
 * Dispatch serves the route it writes to (daemon-pane.ts's `labelShapes`' `writesSteps`): main's
 * since #4400 (53980e0e32). A daemon of main `ff2a10ef`'s shape writes none. */
const WRITES_STEPS = SHAPES.writesSteps;
/** The claim whose agent runs `phase`'s step (workflow/steps.go's `startAgent`): the phase's
 * worker, and in `accepting` the architect that owns the issue, which for the world's issue, its
 * tree's own root, is its own (`acceptingAgent`, `owningArchitect`). */
function stepAgent(phase: StepPhase): string {
  const named = roleFor(phase);
  if (!isLegionRole(named)) throw new Error(`no Legion role runs the step of ${phase}`);
  return roleToken(project, issue, named);
}
/** The latest run of each place of the issue's steps, by the phase whose step it is (record's
 * `step_starts`): the count of starts of its place, the run, the moment its start wrote where
 * this stand-in knows it, and whether it has ended. The world's history ran each seeded phase
 * once, which its seeds recorded (seed-handoffs.ts's `stepsSeed`): ended where the issue stands
 * past it, and open where it is the phase the issue stands in, whose start the seeds wrote at the
 * moment the phase before it ended. The issue's own phase holds one open run, this stand-in's. */
interface KnownRun {
  readonly starts: number;
  readonly run: StepRun;
  readonly startedAt: string | undefined;
  ended: boolean;
}
const RUNS = new Map<StepPhase, KnownRun>();
for (const { phase: ran } of HISTORY?.seeded ?? [])
  if (runsStep(ran))
    RUNS.set(ran, {
      starts: 1,
      run: stepRun(issue, GENERATION, ran, stepAgent(ran), 1),
      startedAt: undefined,
      ended: ran !== PHASE,
    });
if (runsStep(PHASE) && !RUNS.has(PHASE))
  RUNS.set(PHASE, {
    starts: 1,
    run: stepRun(issue, GENERATION, PHASE, stepAgent(PHASE), 1),
    startedAt,
    ended: false,
  });
/** Queues `write` of the record of `run`, as the daemon's workflow queues a `step_record` row in
 * the transaction that starts or ends the run (workflow/steps.go's `startStep`, `endStep` and
 * `reportEndedStep`). A run that names no Dispatch queues none, and neither does a label whose
 * daemon writes none (`WRITES_STEPS`). */
function queueStep(run: StepRun, write: StepWrite): void {
  if (DISPATCH_URL === "" || !WRITES_STEPS) return;
  queueRow({ kind: "step", step: run.id, write });
}
/** The move from `from` to `to` on `trigger`, as the record of the steps holds it
 * (workflow/engine.go's `transition`): the end of the run of `from`'s step (workflow/steps.go's
 * `endOfTransition`, `endStep`), with the result the trigger gives, the place of `to`'s step where
 * the work returns there, the report the completion row of the move names where that row is the
 * one of `from`'s own role, and the session and the model of one read of the run's agent; then
 * the start of the next run of `to`'s step (`startStep`), under the claim that runs it. The
 * session is the claim's as it holds it now, and the model the last its own agent's transcript
 * names (step-records.ts's `answeredModel`): this stand-in's own agent, whose stream production's
 * daemon reads the model off. Another claim's agent never answered this daemon, which booted with
 * the run, so its record names no model, as a restarted daemon's names none until the agent
 * answers again (supervise's `LastAnswer`). */
function moveSteps(from: string, to: string, trigger: string, report: string | undefined): void {
  const now = new Date().toISOString();
  const left = runsStep(from) ? RUNS.get(from) : undefined;
  if (left !== undefined && !left.ended) {
    left.ended = true;
    const { result, returns } = endOf(trigger);
    const session = CLAIMS.get(roleFor(from))?.session ?? "";
    queueStep(
      left.run,
      stepEnd(left.run, {
        ...(left.startedAt === undefined ? {} : { startedAt: left.startedAt }),
        endedAt: now,
        result,
        ...(returns && runsStep(to) ? { returnedTo: STEPS[to].place } : {}),
        ...(report === undefined ? {} : { report }),
        ...(session === "" ? {} : { session }),
        ...(roleFor(from) === role && session !== ""
          ? { model: answeredModel(runDir, session) }
          : {}),
      })
    );
  }
  if (!runsStep(to)) return;
  const starts = (RUNS.get(to)?.starts ?? 0) + 1;
  const run = stepRun(issue, GENERATION, to, stepAgent(to), starts);
  RUNS.set(to, { starts, run, startedAt: now, ended: false });
  queueStep(run, stepStart(run, now));
}

/** Moves the record to `to`, as the engine's transition writes it (engine.go's `transition`):
 * the phase and, where the phase it enters shows another status than the record holds, that
 * status, queued for the run's Dispatch as the engine queues its outbox row (`Engine.status`),
 * with `reason`, the sentence the move posts on the issue before the status (effects.go's
 * `moveReason`: a backward move's), which a move that writes no status posts alone; for a move
 * back (`back`), the implementer's pass, the move's pair, and its round where a review requested
 * the changes (`recordPass`); and the start of the role that works `to` — its handoff cleared
 * (`clearHandoff`), its entry raised, and the task of that entry queued to its claim, which the
 * start makes where the role held none. The slot follows the phase (`keepsSlot`, at each read). */
function enter(to: string, back: "review" | "move" | undefined, reason = ""): void {
  const from = servedPhase;
  const now = new Date().toISOString();
  // The completion row the move names (workflow's `transition`, its `handoff`): the reviewer's for
  // the round's own moves (review.go's `settleRound`) and the requester's for a backward move
  // (engine.go's `backward`); its record is the report of the run the move ends only where it is
  // the row of `from`'s own role.
  const reporter = back === "move" ? role : roleFor(from);
  const report = reporter === roleFor(from) ? HANDOFFS.get(reporter)?.record?.id : undefined;
  moveSteps(
    from,
    to,
    back === "review"
      ? "TriggerReviewRejected"
      : back === "move"
        ? "TriggerBackward"
        : "TriggerReviewApproved",
    report
  );
  servedPhase = to;
  movedAt = now;
  const shown = phaseStatus(to);
  if (shown !== "" && shown !== shownStatus) {
    queueRow({ kind: "status", status: shown, observed: shownStatus, reason });
    shownStatus = shown;
  } else if (reason !== "") queueRow({ kind: "message", body: reason });
  if (back !== undefined) {
    pass += 1;
    backwardMoves[`${from}-${to}`] = (backwardMoves[`${from}-${to}`] ?? 0) + 1;
    if (back === "review") rounds += 1;
  }
  const starts = roleFor(to);
  if (starts === "") return;
  HANDOFFS.delete(starts);
  const claim = CLAIMS.get(starts) ?? claimOf("", "queued", undefined, false);
  CLAIMS.set(starts, claim);
  raise(starts);
  claim.pendingTask = taskOf(to, now);
}

/** The run's issue as its Dispatch answers it, as far as a status write reads it. */
const ShownIssue = z.looseObject({ status: z.string() });
/** An event of the issue as `GET /api/v1/issues/{key}/events` answers it, and a message.created
 * event's payload, as far as the daemon reads them (dispatch/client.go's `issueEventResponse`
 * and `MessageBodiesSince`). */
const IssueEvent = z.looseObject({
  seq: z.number(),
  type: z.string(),
  created_at: z.string(),
  payload: z.unknown(),
});
const MessageCreated = z.looseObject({
  id: z.string().optional(),
  body: z.string().optional(),
  body_truncated: z.boolean().optional(),
});
/** The events the daemon reads in one page (dispatch/client.go's `eventPageLimit`). */
const EVENT_PAGE = 200;
/** How far before a row was queued the outbox looks for its message (outbox.go's
 * `messageReadSkew`). */
const MESSAGE_READ_SKEW_MS = 5_000;
/** The body of every message posted on the run's issue at or after `since` (ms since the epoch),
 * as the daemon reads them (dispatch/client.go's `MessageBodiesSince`): the issue's events
 * newest first, a page of EVENT_PAGE at a time, `before` the last page's last sequence, until one
 * older than `since`; each message.created event's body, read whole from `GET
 * /api/v1/issues/{key}/messages/{id}` where the event carries its first part alone. Throws what
 * the daemon's read throws. */
async function messageBodiesSince(since: number): Promise<string[]> {
  const route = `/api/v1/issues/${encodeURIComponent(issue)}`;
  const bodies: string[] = [];
  for (let before = 0; ; ) {
    // Go's `url.Values.Encode` writes the keys in byte order.
    const query = `${before === 0 ? "" : `before=${before}&`}limit=${EVENT_PAGE}&order=desc`;
    const events = z.array(IssueEvent).parse(await dispatchRead(`${route}/events?${query}`));
    for (const event of events) {
      // Read to the millisecond: Go's RFC 3339 carries up to nine digits of a second.
      const at = Date.parse(event.created_at.replace(/(\.\d{3})\d+/, "$1"));
      if (at < since) return bodies;
      if (event.type !== "message.created") continue;
      const message = MessageCreated.parse(event.payload);
      if (message.body === undefined || message.body === "")
        throw new Error(
          `decode message.created event ${event.seq} for ${issue}: payload body is required`
        );
      bodies.push(
        message.body_truncated === true
          ? z
              .looseObject({ message: z.looseObject({ body: z.string().min(1) }) })
              .parse(
                await dispatchRead(`${route}/messages/${encodeURIComponent(message.id ?? "")}`)
              ).message.body
          : message.body
      );
    }
    const next = events.at(-1)?.seq ?? 0;
    if (events.length < EVENT_PAGE) return bodies;
    if (next <= 0 || (before !== 0 && next >= before))
      throw new Error(`page issue events for ${issue}: non-descending sequence ${next}`);
    before = next;
  }
}
/** Posts `body` on the run's issue as the outbox posts the message of `row`, once
 * (outbox.go's `message`): it reads the issue's messages since a little before the row was
 * queued and posts nothing where one carries the row's marker, a post an earlier attempt made;
 * otherwise it posts the body, then the marker (record's `MessagePost.Posted`), as the daemon's
 * own session (dispatch/client.go's `PostMessage`). */
async function postMessage(row: OutboxRow, body: string): Promise<void> {
  const marker = `<!-- legion-outbox:${row.id} -->`;
  const posted = await messageBodiesSince(row.createdAt - MESSAGE_READ_SKEW_MS);
  if (posted.some((text) => text.includes(marker))) return;
  await dispatchWrite("POST", `/api/v1/issues/${encodeURIComponent(issue)}/messages`, {
    body: `${body}\n\n${marker}`,
  });
}
/** Whether `error` is a refusal the outbox finishes `kind`'s row on rather than retries
 * (outbox.go's `permanentRefusal`): a status write's and a message's, Dispatch's permanent
 * refusal (`permanentRefusal`); a role-holder publication's, a coded 400, which refuses the set
 * it read, and the route's own 404 ISSUE_NOT_FOUND; and a step record's, exactly the refusals its
 * route makes for good (`recordRefused`): the route's 404 ISSUE_NOT_FOUND, a coded 400, 403
 * STEP_WRITER and 409 STEP_OTHER_ISSUE. The router's 404 of a Dispatch older than the route is a
 * wait for both of the last two (outbox.go's `execute`, `waitsForTheRoute`). */
function finishes(kind: OutboxRow["kind"], error: unknown): error is DispatchRefusal {
  if (!(error instanceof DispatchRefusal)) return false;
  if (kind === "step")
    return (
      (error.status === 404 && error.code === "ISSUE_NOT_FOUND") ||
      (error.status === 400 && permanentRefusal(error.status, error.code)) ||
      (error.status === 403 && error.code === STEP_WRITER_CODE) ||
      (error.status === 409 && error.code === STEP_OTHER_ISSUE_CODE)
    );
  if (kind !== "holders") return permanentRefusal(error.status, error.code);
  return (
    error.code !== "" &&
    (error.status === 400 || (error.status === 404 && error.code === "ISSUE_NOT_FOUND"))
  );
}
/** Runs `row` as the daemon's outbox runs its kind (packages/daemon/internal/daemon/outbox.go),
 * and answers whether it is finished. A status (`status`): it reads the issue and writes nothing
 * where Dispatch shows neither the status the move observed nor the one it carries — a change
 * someone made since, which stands (`statusMoved`); it posts the move's sentence first where the
 * row carries one (`postMessage`), then reads the issue again, and writes the status through
 * `PATCH /api/v1/issues/{key}` unless Dispatch shows it already (`writeStatus`). A sentence
 * Dispatch refuses for good costs the issue no status: the status is written all the same. A read
 * or a write of the status Dispatch refuses for good — the issue's coded 404 among them — finishes
 * the row refused, writing nothing more (`permanentRefusal`, `finishRefused`), where the daemon
 * notifies the architect, whom no run boots. The write carries no `expected_sequence`, as the
 * outbox's does: the read before it is of the same moment, and a fenced write refused for an
 * agent's own write of that moment would stand in the run's Dispatch log as a refusal of that
 * agent's (health.ts's rule 16). A message (`message`) is posted (`postMessage`), and dropped when
 * Dispatch refuses it for good. A publication (`holders`) puts the live set (`liveHolders`)
 * through `PUT /api/v1/issues/{key}/role-holders` (`publishRoleHolders`), and is dropped when
 * Dispatch refuses that set. Each drop writes the daemon's line on stderr. Each write is the
 * daemon's own session's, `legion-daemon:<PROJECT>` (the session the world's seeds write each
 * status as: seed-handoffs.ts's DAEMON). Throws why a row that is not finished failed. */
async function runRow(row: OutboxRow): Promise<boolean> {
  const route = `/api/v1/issues/${encodeURIComponent(issue)}`;
  switch (row.kind) {
    case "step":
      try {
        await dispatchWrite("PUT", `${route}/steps/${encodeURIComponent(row.step)}`, row.write);
      } catch (error) {
        if (!finishes(row.kind, error)) throw error;
        console.error(
          `daemon-standin: the run's Dispatch refused step record ${row.step} of ${issue} for good, and it is dropped: ${error.message}`
        );
      }
      return true;
    case "message":
      try {
        await postMessage(row, row.body);
      } catch (error) {
        if (!finishes(row.kind, error)) throw error;
        console.error(
          `daemon-standin: the run's Dispatch refused message row ${row.id} of ${issue} for good, and it is dropped: ${error.message}`
        );
      }
      return true;
    case "holders": {
      const asked = holdersAsked;
      try {
        await dispatchWrite("PUT", `${route}/role-holders`, { holders: liveHolders() });
      } catch (error) {
        if (!finishes(row.kind, error)) throw error;
        console.error(
          `daemon-standin: the run's Dispatch refused the role-holders publication of ${issue}, and it is dropped; the set it holds is the last one it took: ${error.message}`
        );
      }
      return asked === holdersAsked;
    }
    case "status":
      try {
        let shown = ShownIssue.parse(await dispatchRead(route)).status;
        if (shown !== row.observed && shown !== row.status) return true;
        if (row.reason !== "") {
          try {
            await postMessage(row, row.reason);
          } catch (error) {
            if (!finishes(row.kind, error)) throw error;
            console.error(
              `daemon-standin: the run's Dispatch refused the reason of status row ${row.id} of ${issue} for good; the status is written all the same: ${error.message}`
            );
          }
          shown = ShownIssue.parse(await dispatchRead(route)).status;
        }
        if (shown !== row.status) await dispatchWrite("PATCH", route, { status: row.status });
      } catch (error) {
        if (!finishes(row.kind, error)) throw error;
        console.error(
          `daemon-standin: the run's Dispatch refused status row ${row.id} of ${issue} (${row.status}) for good, and it is finished refused: ${error.message}`
        );
      }
      return true;
  }
}
/** Runs every row the run's Dispatch is owed, oldest first, as the daemon's outbox runs its lanes:
 * a status or a message that fails holds every later one behind it, as the issue's writes run in
 * the order they were made; the role-holder publication runs on a lane of its own; and the step
 * records run on the record lane, in their order, so a record that fails holds every later record
 * and no status, message or publication (record's `LaneRecord`). A row not finished stays owed,
 * with why, for the next run, where the outbox waits out a backoff: the state's
 * `pendingStatusWrites` lists each status so owed. Each request this stand-in answers runs the
 * outbox before its answer, so a `dispatch read` after a move shows the status the record names:
 * the record moved and Dispatch kept the status of the phase it had left. One run at a time, so
 * no row is run twice at once. */
let outboxTurn: Promise<void> = Promise.resolve();
function runOutbox(): Promise<void> {
  outboxTurn = outboxTurn.then(async () => {
    let held = false;
    let recordsHeld = false;
    for (const row of [...OUTBOX]) {
      if (row.kind === "step" ? recordsHeld : held && row.kind !== "holders") continue;
      try {
        if (await runRow(row)) OUTBOX.splice(OUTBOX.indexOf(row), 1);
      } catch (error) {
        row.attempts += 1;
        row.lastError = error instanceof Error ? error.message : String(error);
        if (row.kind === "step") recordsHeld = true;
        else if (row.kind !== "holders") held = true;
      }
    }
  });
  return outboxTurn;
}

/** The contract the label's plugin declares at its registration (`pluginContract`: its
 * `daemonContract`, packages/contracts/src/daemon-contract.ts, read by
 * packages/pi-legion/src/claim-session.ts), which every claim of the issue's pod records, since
 * they all load the one plugin: read from the label's checkout until the agent registers, and the
 * registration's own from then on. A package holds it as `legion.daemonApiVersion` where a pack
 * wrote it, and a checkout of main since #5399 holds none, so the number is the one
 * `daemonContract` counts from the checkout's own fixtures, as the plugin run from that checkout
 * reads it. 0 is the daemon's word for no contract recorded, which is a checkout with no plugin
 * package. */
const PluginPackage = z.looseObject({
  legion: z.looseObject({ daemonApiVersion: z.number().int().nonnegative().optional() }).optional(),
});
let pluginContract = (() => {
  const file = `${CHECKOUT}/packages/pi-legion/package.json`;
  if (!existsSync(file)) return 0;
  return daemonContract(PluginPackage.parse(JSON.parse(readFileSync(file, "utf8"))), CHECKOUT);
})();

/** A `claims/ready` as far as the record reads it: the capability report a session sends on one
 * of its own (`claim.CapabilityReport`). The daemon's own normalising of the rows (the live
 * table's order, a name it does not know dropped, a live row not reported marked open, each
 * detail cleaned of credentials and control characters and cut: `capabilities.Normalize`,
 * `cleanDetail`) is not modelled: a row is `ok` or open as the session reported it, an open one
 * with the daemon's own words where it gave none (`capabilities.NewRow`). A residual of the
 * class search (the architect's ruling of 2026-11-19T05:18Z): no agent of round 25 sent a report
 * past its boot's, and a run that sends one is unhealthy from that call on (health.ts's
 * `capabilityReport`). */
const ReadyReport = z.looseObject({
  capabilities: z
    .looseObject({
      measuredAt: z.string(),
      rows: z.array(
        z.looseObject({ name: z.string().min(1), ok: z.boolean(), detail: z.string() })
      ),
    })
    .optional(),
});

/** The design gate as the run's Dispatch holds the issue's spec now (dispatch/model's
 * `ArtifactApproval`), read at each state read since a write of the spec moves it: the document,
 * its latest version, the version a person approved — `approved` at the latest, `stale` at an
 * older one — and the reason of a request for changes at the latest. Undefined for a run that
 * seeded no history, and for a spec with no version, which is no registered gate. */
async function gateNow(): Promise<GateFacts | undefined> {
  if (HISTORY === undefined) return undefined;
  const { approval } = Spec.parse(
    await dispatchRead(`/api/v1/artifacts/${encodeURIComponent(HISTORY.spec)}`)
  );
  if (approval === undefined || approval.latest_version < 1) return undefined;
  const approved = approval.state === "approved" || approval.state === "stale";
  return {
    artifactId: HISTORY.spec,
    currentVersion: approval.latest_version,
    approvedVersion: approved && approval.version !== undefined ? approval.version : null,
    changesReason:
      approval.state === "changes_requested" && approval.version === approval.latest_version
        ? (approval.reason ?? "")
        : undefined,
  };
}

/** The state document of the run's issue as the world stands now (standin-record.ts's
 * `stateBody`), read through this checkout's own contract so a body it does not describe is never
 * served. Where the label's daemon shapes its bodies (`DAEMON_CONTRACT`) it is the body that daemon
 * serves a caller at its own contract, marked with that contract and the claims' lowest and each
 * claim with its update state (`RecordFacts`' `daemonContract`); its repositories block is the one
 * of the label's daemon, the repositories the live records name or the project's mapping
 * (`REPOSITORIES_LIVE`). Throws where the world cannot be read — the run's Dispatch not answering
 * for the spec. */
const servedState = async (): Promise<z.infer<typeof LegionStateResponse>> => {
  const pullRequest = settled();
  await runOutbox();
  const workers: Record<string, RowFacts> = {};
  for (const [named, claim] of CLAIMS) {
    if (named === "architect") continue;
    const handoff = HANDOFFS.get(named);
    workers[named] = { claim, handoffCommit: handoff?.commit, handoffRecord: handoff?.record };
  }
  const body = stateBody(
    {
      project,
      issue,
      phase: servedPhase,
      status: shownStatus,
      statusWrites: OUTBOX.flatMap((row) => (row.kind === "status" ? [row] : [])),
      repo,
      defaultBranch: base,
      pluginContract,
      daemonContract: DAEMON_CONTRACT,
      repositoriesLive: REPOSITORIES_LIVE,
      architect: CLAIMS.get("architect"),
      workers,
      pass,
      rounds,
      backwardMoves,
      pullRequest,
      fixAttempts: FIX_ATTEMPTS,
      gate: await gateNow(),
      admittedAt,
      entries: Object.fromEntries(ENTRIES),
      movedAt,
      now: new Date().toISOString(),
    },
    startedAt
  );
  const served = LegionStateResponse.parse(body);
  // This checkout's contract keeps no member it does not declare, and the count of fix attempts
  // a label of main `ff2a10ef`'s shape serves is that label's own, which its contract requires;
  // so is the mapping a label before #4493 serves as its repositories block, whose `owners`,
  // `mapped` and `refused` this checkout's contract, main's since #4493, does not declare.
  const pull = served.issues[issue]?.pullRequest;
  if (pull !== undefined && FIX_ATTEMPTS !== undefined)
    Object.assign(pull, { fixAttempts: FIX_ATTEMPTS });
  if (!REPOSITORIES_LIVE) Object.assign(served, { repositories: body.repositories });
  return served;
};

/** A world that cannot be read, answered 500 with why, as a daemon that cannot build its state
 * answers, never with a record of what the world last said. */
function unbuilt(error: unknown): Response {
  return json(500, {
    error: `the state could not be built: ${error instanceof Error ? error.message : String(error)}`,
  });
}

/** The answer to a read of the state or the record whose caller states `stated` in
 * `Legion-Plugin-Contract`, where the label's daemon would shape the body below its own and this
 * stand-in does not: undefined where it serves the daemon's own body. A daemon that shapes nothing
 * (`DAEMON_CONTRACT` undefined) serves every caller its own. One that shapes places a caller
 * (shaping.go's `contractsFor`) by the contract its header states, and one that states none, or no
 * positive whole number, by the contract the claims of its pod record (`podContracts`), here the
 * one every claim of the issue records (`pluginContract`); a caller placed at or above the
 * daemon's own contract, or by claims recording none, is served the daemon's own body. The
 * shaping of a body down to a lower contract (shaping.go's `shapeAnswer`, its bands and the
 * records of contract-shapes.json) is not modelled: such a read is answered 500 in this stand-in's
 * own words, never with a body of the daemon's own contract, and so is a headerless read whose
 * claims record a contract below the daemon's that the daemon counts for nothing (one at or above
 * `shapedAnswersContract`, where it serves its own body), since this refuses rather than reads
 * that bound. Unreachable: a read is placed below the daemon's contract only by a plugin that
 * states a lower `Legion-Plugin-Contract`, or, stating none, by a pod whose claims record a lower
 * one, and every agent of a run loads its own label's plugin, a build of that label's checkout
 * (rig.sh's load_label), whose client states that build's contract on every request (pi-shared's
 * daemon-client.ts) and registers it as the claim's (`claims/register`'s `pluginContract`), and a
 * checkout's plugin and daemon declare one number. */
function unshaped(stated: string | null): Response | undefined {
  if (DAEMON_CONTRACT === undefined) return undefined;
  // As Go's strconv.Atoi reads it: digits with an optional sign; no positive whole number is no
  // statement at all (`statedContract`), and the caller is placed by its pod.
  const declared = stated !== null && /^[+-]?\d+$/.test(stated) ? Number(stated) : Number.NaN;
  const contract =
    Number.isSafeInteger(declared) && declared > 0 ? declared : pluginContract || DAEMON_CONTRACT;
  if (contract >= DAEMON_CONTRACT) return undefined;
  return json(500, {
    error: `daemon-standin models no body shaped down to contract ${contract}: the label's daemon speaks ${DAEMON_CONTRACT}, and this stand-in serves its own body alone`,
  });
}

/** `GET /legion/v1/state` for a caller stating `stated`: the whole state document
 * (`servedState`). */
const stateAnswer = async (stated: string | null): Promise<Response> => {
  const refused = unshaped(stated);
  if (refused !== undefined) return refused;
  try {
    return json(200, await servedState());
  } catch (error) {
    return unbuilt(error);
  }
};

/** `GET /legion/v1/issues/<key>/record` for a caller stating `stated`, `key` as the path writes
 * it: the one issue's record the state holds under `issues.<key>` (server.go's
 * `issueRecordRoute`), and 404 RECORD_NOT_FOUND in the words daemon-pane.ts's `RECORD_NOT_FOUND`
 * restates for a key it holds no record of, every key but the run's issue. Only a label whose
 * daemon states its contract registers the route (`ROUTES`), and `labelShapes` holds its source
 * to those words. */
const recordAnswer = async (key: string, stated: string | null): Promise<Response> => {
  const refused = unshaped(stated);
  if (refused !== undefined) return refused;
  let named = key;
  try {
    named = decodeURIComponent(key);
  } catch {
    // A key whose escapes do not decode names no issue: it is answered as written.
  }
  try {
    const held = (await servedState()).issues[named];
    return held === undefined
      ? failure(404, RECORD_NOT_FOUND.code, `${RECORD_NOT_FOUND.message}${named}`)
      : json(200, held);
  } catch (error) {
    return unbuilt(error);
  }
};

/** The refusal of a completion naming handoff record `id` at `version`, read back from the run's
 * own Dispatch as the daemon reads it (handoff.go's `handoffRecord`), or undefined where the
 * record is this issue's, of the phase's handoff word, at that version and written by `session`,
 * the claim's own. A record Dispatch
 * answers 404 for, or refuses with a code that says the id names nothing it holds
 * (dispatch.PermanentRefusal: a 4xx with a code, but 401, 403, 408, 429 and a contended
 * parent), is HANDOFF_RECORD_NOT_FOUND, 404; any other answer, or none, DISPATCH_READ_FAILED,
 * 502; a record of another issue, phase or version HANDOFF_RECORD_MISMATCH, 409; one another
 * session wrote HANDOFF_SESSION_MISMATCH, 403; and a run that
 * names no Dispatch DISPATCH_UNAVAILABLE, 500. Each sentence is the daemon's, the Dispatch error
 * in it as Go prints one (`<code>: <message>`). */
async function handoffRecord(
  id: string,
  version: number,
  session: string
): Promise<Response | undefined> {
  const write = `\`dispatch handoff-write --issue ${issue} --phase ${HANDOFF_WORD} --body-file <file>\``;
  const read = `\`dispatch handoff-read --issue ${issue} --phase ${HANDOFF_WORD}\``;
  if (DISPATCH_URL === "") return failure(500, "DISPATCH_UNAVAILABLE", "Dispatch is unavailable");
  const unanswered = (why: string) =>
    failure(
      502,
      "DISPATCH_READ_FAILED",
      `Dispatch did not answer the read of handoff record ${id} (${why}): complete again once Dispatch answers; this completion changed nothing`
    );
  let answer: Response;
  try {
    const token =
      DISPATCH_TOKEN_FILE === "" ? "" : (await Bun.file(DISPATCH_TOKEN_FILE).text()).trim();
    answer = await fetch(`${DISPATCH_URL}/api/v1/handoffs/${encodeURIComponent(id)}`, {
      headers: { Authorization: `Bearer ${token}` },
    });
  } catch (error) {
    return unanswered(error instanceof Error ? error.message : String(error));
  }
  const text = await answer.text();
  if (!answer.ok) {
    let refusal: z.infer<typeof DispatchError> = {};
    try {
      refusal = DispatchError.parse(JSON.parse(text));
    } catch {
      refusal = { error: text.trim() };
    }
    const code = refusal.code ?? "";
    const message = refusal.error || answer.statusText;
    const why = code === "" ? message : `${code}: ${message}`;
    if (!permanentRefusal(answer.status, code) && answer.status !== 404) return unanswered(why);
    return failure(
      404,
      "HANDOFF_RECORD_NOT_FOUND",
      `Dispatch has no handoff record ${id} (${why}): name the record ${read} shows, or write one with ${write}, then complete again; this completion changed nothing`
    );
  }
  const record = HandoffRecord.safeParse(JSON.parse(text));
  if (!record.success)
    return unanswered(`a handoff record no reader can take: ${record.error.message}`);
  const held = record.data;
  if (held.issue_key !== issue || held.phase !== HANDOFF_WORD || held.version !== version)
    return failure(
      409,
      "HANDOFF_RECORD_MISMATCH",
      `handoff record ${id} is version ${held.version} of ${held.issue_key}'s ${held.phase} handoff, and this completion is of ${issue}'s ${HANDOFF_WORD} handoff at version ${version}: name the record ${read} shows; this completion changed nothing`
    );
  // The record is the claim's own session's (handoff.go's `handoff.Author.IsSession(session)`):
  // one another session wrote is refused 403, and one the daemon's own session wrote, a record
  // carried from before handoffs moved to the Dispatch issue, with its own sentence. The claim's
  // session is the one its grant was minted for, which is the one it registered: measured on
  // INFRA-764 round 25's 40 worker runs, every one of the 31 handoff records an agent wrote
  // names as its author the session that agent registered with the stand-in. A record a seed
  // wrote for the role an agent now plays is another session's, the seed's, so a completion
  // naming it is refused as production refuses a claim naming a record an earlier session of
  // its role wrote, and told to write its own.
  const author = held.author;
  if (author?.kind === "session" && author.id === session) return undefined;
  const named = `handoff record ${id} (${HANDOFF_WORD} version ${held.version})`;
  if (author?.kind === "session" && author.id === DAEMON_SESSION)
    return failure(
      403,
      "HANDOFF_SESSION_MISMATCH",
      `${named} was written by the daemon's own session ${author.id}, as a record carried from before handoffs moved to the Dispatch issue is, not by this claim's session ${session}: read it with ${read}, write it again with ${write} from this session, then complete naming the new record; this completion changed nothing`
    );
  return failure(
    403,
    "HANDOFF_SESSION_MISMATCH",
    `${named} was written by ${author?.kind ?? ""} ${author?.id ?? ""}, not by this claim's session ${session}: write the handoff again with ${write} from this session, then complete naming the new record; this completion changed nothing`
  );
}

/** `POST /legion/v1/claims/turn-outcome` (`turnOutcome` in packages/daemon/internal/api/claims.go):
 * a body of neither caller or of both, a sequence or a generation of 0, and an outcome neither
 * `clean` nor `refused` are refused 400 by the body's contract; a boot token that is not the
 * launch's is refused `InvalidBootToken`, and a report under it for another generation than the
 * launch's, 1, `StaleGeneration`; a claim and secret the claim does not authenticate
 * (`authenticates`) `InvalidSecret`. A report is then recorded in the window (`TURN_OUTCOMES`),
 * and the answer is the run of refused turns the window ends with (supervise's `RefusedRun`) and
 * whether it reached the launch failure limit, which fails the claim: from then on the claim's
 * secret authenticates nothing (`capabilityHeld`), so the plugin's next report, which names the
 * claim with its secret, is refused `InvalidSecret`, as is every other route that reads the
 * secret. Not modelled: a report under the boot token on a failed claim, which the daemon hands
 * to the failed machine and refuses with the machine's own sentence for a request in a failed
 * claim. Unreachable: only the Claude bridge reports under a boot token, and every agent a
 * scenario boots is Oh My Pi with the label's pi-legion plugin, whose report names the claim and
 * its secret (packages/pi-legion/src/turn-outcome.ts); a run whose claim failed is unhealthy
 * whatever comes after (health.ts's `failedClaim`), so this answers such a report with its own
 * sentence and names no daemon's. */
function turnOutcome(body: unknown): Response {
  const parsed = LegionTurnOutcomeRequest.safeParse(body);
  if (!parsed.success) return json(400, { error: parsed.error.message });
  const report = parsed.data;
  if ("bootToken" in report) {
    if (report.bootToken !== expectedBootToken) return refuse(INVALID_BOOT_TOKEN);
    if (report.generation !== 1) return refuse(STALE_GENERATION);
    if (!capabilityHeld)
      return json(500, {
        error: "daemon-standin models no boot-token report on a claim the daemon failed",
      });
  } else if (report.claimToken !== ROLE_TOKEN || !authenticates(report.secret)) {
    return refuse(INVALID_SECRET);
  }
  const turn = {
    generation: report.generation,
    sequence: report.sequence,
    refused: report.outcome === "refused",
  };
  const at = TURN_OUTCOMES.findIndex(
    (kept) =>
      kept.generation > turn.generation ||
      (kept.generation === turn.generation && kept.sequence >= turn.sequence)
  );
  const same = TURN_OUTCOMES[at];
  if (same !== undefined && same.generation === turn.generation && same.sequence === turn.sequence)
    TURN_OUTCOMES[at] = turn;
  else TURN_OUTCOMES.splice(at === -1 ? TURN_OUTCOMES.length : at, 0, turn);
  TURN_OUTCOMES.splice(0, Math.max(0, TURN_OUTCOMES.length - TURN_OUTCOME_LIMIT));
  let refusedRun = 0;
  for (let i = TURN_OUTCOMES.length - 1; i >= 0 && TURN_OUTCOMES[i]?.refused === true; i--)
    refusedRun++;
  const failed = refusedRun >= TURN_OUTCOME_LIMIT;
  if (failed) capabilityHeld = false;
  endTurn(OWN, new Date().toISOString(), failed);
  // The claim the daemon fails holds its role no longer, a change of the role's holder its write
  // publishes (store's `putClaim`).
  if (failed) queueHolders();
  return json(200, { refusedRun, failed });
}

/** Answers one POST: the response, the grant it minted, and, for a completion that reached the
 * phase read, the phase it was answered on. Its cases are the routes this stand-in answers of
 * those its label's daemon registers (`ROUTES`, which `fetch` holds every request to first).
 * One route a phase worker's tool can reach is registered by both labels' daemons and answered
 * by no case: `claims/ready-for-update`, the tool's `ready_for_update`. It answers the update
 * message a pod replacement sends, and the rig opens no update round, no prompt names one, and
 * no kept run called it, so an agent reaches it only with a round it made up; the daemon's answer
 * to that is a sentence its supervisor builds in an unexported function, which this does not
 * restate. Such a call is answered 404, which health reads as a world the daemon would not have
 * served, so the run is unhealthy and never scored.
 *
 * A project's pause and its resume (main's #4115: `POST /legion/v1/project/pause` and
 * `/legion/v1/project/resume`, the state's `paused` and `lastPause`, and the operator routes a
 * pause refuses while it stands) are not modelled. Unreachable: only the controller's `legion`
 * tool sends a pause or a resume (pi-legion's tools.ts, whose `controller` ops alone hold `pause`
 * and `resume`), and no scenario boots a controller (rig.sh's worker_boots), so no run's state
 * holds a pause and no agent of a run can send one. */
async function handle(
  path: string,
  body: unknown
): Promise<{ response: Response; mintedGrantId?: string; phase?: string }> {
  switch (path) {
    case "/legion/v1/claims/turn-outcome":
      return { response: turnOutcome(body) };
    case "/legion/v1/claims/release-records": {
      // `releaseRecords` there: the body's contract, then the claim's secret, then the project's
      // records since the time named, of which the rig keeps none.
      const parsed = LegionReleaseRecordsRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      if (parsed.data.claimToken !== ROLE_TOKEN || !authenticates(parsed.data.secret)) {
        return { response: refuse(INVALID_SECRET) };
      }
      return { response: json(200, { releases: [], resumes: [], holds: [] }) };
    }
    case "/legion/v1/claims/register": {
      if (stringField(body, "bootToken") !== expectedBootToken) {
        return { response: refuse(INVALID_BOOT_TOKEN) };
      }
      // The claim is the registering session's from here, under the contract its plugin
      // declares (`register`, `pluginContract`), and the session holds the claim's role, which
      // the write of the claim publishes (store's `putClaim`).
      register(OWN, stringField(body, "sessionId"));
      pluginContract = numberField(body, "pluginContract") ?? pluginContract;
      queueHolders();
      return {
        response: json(200, {
          claimToken: ROLE_TOKEN,
          tree: issue,
          issue,
          role,
          generation: 1,
          secret: SESSION_SECRET,
          promptAgents: [],
        }),
      };
    }
    case "/legion/v1/claims/ready": {
      if (!authenticates(stringField(body, "secret"))) {
        return { response: refuse(INVALID_SECRET) };
      }
      // The agent can be prompted, and its turn begins (`ready`); a ready of the session's own
      // carries its capability report, which the claim keeps (`ReadyReport`), and a ready that
      // carries none, a boot's, first queues the publication of the issue's role holders (api's
      // `ready`, `roleHoldersChanged`).
      const report = ReadyReport.safeParse(body).data?.capabilities;
      if (report === undefined) queueHolders();
      ready(OWN, new Date().toISOString());
      if (report !== undefined)
        OWN.capabilities = {
          measuredAt: report.measuredAt,
          ok: report.rows.filter((row) => row.ok).map((row) => row.name),
          open: report.rows
            .filter((row) => !row.ok)
            .map((row) => ({ name: row.name, detail: row.detail.trim() || "no detail reported" })),
        };
      return { response: new Response(null, { status: 204 }) };
    }
    case "/legion/v1/claims/activity": {
      if (!authenticates(stringField(body, "secret"))) {
        return { response: refuse(INVALID_SECRET) };
      }
      return { response: new Response(null, { status: 204 }) };
    }
    case "/legion/v1/grants": {
      const parsed = LegionGrantRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      if (!authenticates(parsed.data.secret)) {
        return { response: refuse(INVALID_SECRET) };
      }
      const grantId = randomUUID();
      const expiresAt = Date.now() + GRANT_TTL_MS;
      grants.set(grantId, { expiresAt, sessionId: parsed.data.sessionId });
      return {
        response: json(200, { grantId, expiresAt: new Date(expiresAt).toISOString() }),
        mintedGrantId: grantId,
      };
    }
    case COMPLETE_PATH: {
      const parsed = CompleteRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      const refused = resolveGrant(parsed.data.grantId);
      if (refused) return { response: refused };
      // A label whose daemon reads `ready` again only to refuse it (`RETIRES_READY`, main's since
      // #3659) refuses `ready: true` once the grant is redeemed, before it reads the issue, in its
      // own words (handoff.go's `readyRetired`), and ignores `ready: false`. A daemon before it is
      // answered as this answered every label's `ready` (`CompleteRequest`).
      if (RETIRES_READY && parsed.data.ready === true)
        return { response: failure(400, READY_RETIRED.code, READY_RETIRED.message) };
      // Both the route and the workflow read the phase the issue stands in now, the one a state
      // read serves, and never the phase this role was booted in. The earlier HANDOFF_NO_RUN
      // fence of the route is not reached: a rig half is a live registered worker serving
      // generation 1, so the route attributes its completion to a run; nor is the entry fence
      // (the module doc says why).
      const phase = served();
      // The route's rule on the verdict, before the fact (handoff.go's `answerCompletion`): a
      // tester reports `pass` or `fail`, and no other role reports one, each refused 400 in the
      // words every daemon the rig runs gives (daemon-pane.ts's VERDICT_REFUSALS, held to the
      // label's source at boot). The `legion` tool sends whatever verdict the agent names, and
      // the reviewer's prompt names this refusal (prompts/go/reviewer.md), so either is an agent's
      // own call; this took both.
      const { verdict } = parsed.data;
      const verdictRefusal =
        role === "tester"
          ? verdict === "pass" || verdict === "fail"
            ? undefined
            : VERDICT_REFUSALS.tester
          : verdict === ""
            ? undefined
            : VERDICT_REFUSALS.other;
      if (verdictRefusal !== undefined)
        return {
          phase,
          response: failure(400, verdictRefusal.code, verdictRefusal.message),
        };
      const { handoffId, handoffVersion, commit } = parsed.data;
      const namesRecord = handoffId !== undefined || handoffVersion !== undefined;
      // The route (packages/daemon/internal/api/handoff.go), as the label's daemon has it
      // (`RECORD_ROUTE`). A phase's writer is the role the workflow starts on it. A `left` route
      // (this checkout's) holds the completion to the phase it is of (`phase.Of`): the served
      // phase when this role works it, and otherwise the one it left, its own; the completion is
      // record-backed when that phase ends with a record and it is the served phase or the
      // completion names a record. An `issue` route (an older daemon's) holds it to the served
      // phase, and it is record-backed exactly when the issue stands in this role's own phase and
      // that phase ends with a record.
      const of = RECORD_ROUTE === "left" ? ROLE_PHASE : phase;
      const recordBacked =
        RECORD_ROUTE === "left"
          ? HANDOFF_WORD !== "" && (phase === ROLE_PHASE || namesRecord)
          : phase === ROLE_PHASE && HANDOFF_WORD !== "";
      if (recordBacked) {
        // A record it names is read back from the run's own Dispatch and held to the issue, the
        // word and the version (`handoffRecord`).
        if (handoffId === undefined || handoffVersion === undefined) {
          const write = `\`dispatch handoff-write --issue ${issue} --phase ${HANDOFF_WORD} --body-file <file>\``;
          return {
            phase,
            response: failure(
              400,
              "HANDOFF_RECORD_REQUIRED",
              `phase ${of} of ${issue} ends with its ${HANDOFF_WORD} handoff recorded on the Dispatch issue, and this completion names no record (handoffId and a positive handoffVersion): write it with ${write}, then complete again; this completion changed nothing`
            ),
          };
        }
        const unread = await handoffRecord(
          handoffId,
          handoffVersion,
          grants.get(parsed.data.grantId)?.sessionId ?? ""
        );
        if (unread !== undefined) return { phase, response: unread };
      } else if (!RECORD_PHASES.includes(of) && namesRecord) {
        return {
          phase,
          response: failure(
            409,
            "HANDOFF_RECORD_MISMATCH",
            `phase ${of} of ${issue} ends with no handoff record, and this completion names one: complete without handoffId and handoffVersion; this completion changed nothing`
          ),
        };
      }
      // The fact (intake's `ApplyFact`): one phase's completion is identified by where the
      // issue stands — its phase and its pass through it — with the role and what it reported,
      // the record a record-backed completion names or else its commit, and the verdict
      // (handoff.go's `eventID`), and it is marked processed before the workflow reads it. So a
      // call that repeats one the workflow already answered, taken or refused, is answered 409
      // HANDOFF_ALREADY_RECORDED and reaches no workflow, and the completions the world's seeds
      // stand for are such facts too (`FACTS`). This answered a repeat as its first call was
      // answered, a second success among them, which production gives none of.
      const fact = factOf(
        role,
        phase,
        pass,
        recordBacked ? (handoffId ?? "") : (commit ?? ""),
        parsed.data.verdict
      );
      if (FACTS.has(fact)) {
        const received = recordBacked
          ? `naming handoff record ${handoffId} (${HANDOFF_WORD} version ${handoffVersion})`
          : `at commit ${commit ?? ""}`;
        return {
          phase,
          response: failure(
            409,
            "HANDOFF_ALREADY_RECORDED",
            `the ${role} completion of phase ${phase} (pass ${pass} through it) ${received} was already received; this call changed nothing`
          ),
        };
      }
      FACTS.add(fact);
      // The workflow (packages/daemon/internal/workflow/engine.go, `handoff`): a late role's
      // completion is answered by the label's engine (LATE_ROLE): an older engine refuses every
      // one, and this checkout's records it unless the role's row holds a completion
      // (`leftItsPhase`'s `row.Completed()`), which is a row the world's seeds completed, one
      // the rig says completed with a commit, and one whose completion was taken here
      // (`completed`). The refusal's code and message are the daemon's own, read from its code
      // at boot (NOT_CURRENT_PHASE), because the skill has the worker quote them to its
      // architect.
      if (phase !== ROLE_PHASE) {
        if (LATE_ROLE === "refused" || completed(role)) {
          if (NOT_CURRENT_PHASE === undefined)
            throw new Error(`no refusal was read at boot for ${role}'s completion in ${phase}`);
          return {
            phase,
            response: failure(
              409,
              NOT_CURRENT_PHASE.code,
              NOT_CURRENT_PHASE.message.replaceAll(PHASE_SLOT, phase).replaceAll(ISSUE_SLOT, issue)
            ),
          };
        }
        // `leftItsPhase`: the completion is of the phase this role left (`phase.Of`, the
        // role's own), and a phase that ends with a record requires the record it names.
        if (HANDOFF_WORD !== "" && handoffId === undefined) {
          return {
            phase,
            response: failure(
              409,
              "HANDOFF_RECORD_REQUIRED",
              `the ${role}'s completion of phase ${ROLE_PHASE} of ${issue} names no handoff record: write it with \`dispatch handoff-write --issue ${issue} --phase ${HANDOFF_WORD} --body-file <file>\`, then complete again; this completion changed nothing`
            ),
          };
        }
      }
      // A phase that ends with a record ends with a new one: the record the role's row last
      // named, its previous completion's or the one its seed stands for (`LAST_HANDOFF`), means
      // this completion wrote none (the engine's HANDOFF_NOT_NEW). A repeat of a whole
      // completion never reaches here (HANDOFF_ALREADY_RECORDED above): this is the same record
      // under another verdict, or in a phase the issue has since entered.
      if (HANDOFF_WORD !== "" && handoffId !== undefined && handoffId === LAST_HANDOFF.get(role)) {
        return {
          phase,
          response: failure(
            409,
            "HANDOFF_NOT_NEW",
            `the ${role} named handoff record ${handoffId} for its previous phase of ${issue}; write this phase's handoff with \`dispatch handoff-write --issue ${issue} --phase ${HANDOFF_WORD} --body-file <file>\` before completing`
          ),
        };
      }
      // Past it, the rating of the plan (engine.go's `handoff`, workflow/plan.go's
      // `planRatingRefusal`), where the label's workflow refuses one (RATING_REFUSED): only the
      // completion that ends the retro takes a rating, and no run of this rig stands in retro (the
      // module doc), so a completion that carries one is refused, naming the phase it is of, the
      // role's own (`phase.Of`), and nothing of it is recorded: its fact is left unspent, so the
      // same completion sent again without the rating is taken under the same key. The contract
      // this parses with holds `planRating` and `planReason` together.
      if (RATING_REFUSED && parsed.data.planRating !== undefined) {
        FACTS.delete(fact);
        const what = filled(PLAN_RATING_UNEXPECTED.what, ROLE_PHASE, issue);
        return {
          phase,
          response: failure(
            400,
            PLAN_RATING_UNEXPECTED.code,
            filled(PLAN_RATING_UNEXPECTED.message, what)
          ),
        };
      }
      // The store of a label that takes a body holding U+0000 fails such a completion where the
      // engine writes its row (`storeFails`): its route answers the failed apply, and nothing of
      // it is recorded, so its fact is unspent.
      if (storeFails(parsed.data.summary, commit, handoffId)) {
        FACTS.delete(fact);
        return {
          phase,
          response: failure(500, FACT_APPLY_FAILED.code, FACT_APPLY_FAILED.completion),
        };
      }
      // A taken completion is written on the role's row, its commit as the row's handoff commit
      // and its record as the row's handoff record (engine.go's `handoff` writes both from the
      // completion, a commit or a record it does not name included), either of which completes
      // the row (`row.Completed()`), and it moves nothing: the review decides the round, and the
      // record moves when the world reports a decision that ends it (`servedPhase`,
      // `roundOutcome`).
      if (HANDOFF_WORD !== "" && handoffId !== undefined) LAST_HANDOFF.set(role, handoffId);
      HANDOFFS.set(role, {
        commit,
        record:
          handoffId === undefined || handoffVersion === undefined
            ? undefined
            : { id: handoffId, version: handoffVersion },
      });
      // A completion of the phase this role has left is the report of the run of that phase's
      // step the move ended (engine.go's `handoff`, workflow/steps.go's `reportEndedStep`): the
      // record it names, written onto that run's record alone, where its latest run has ended.
      const reported = runsStep(ROLE_PHASE) ? RUNS.get(ROLE_PHASE) : undefined;
      if (phase !== ROLE_PHASE && handoffId !== undefined && reported?.ended === true)
        queueStep(reported.run, stepReport(reported.run, handoffId));
      return { response: json(200, {}), phase };
    }
    case "/legion/v1/phase/backward": {
      const parsed = LegionPhaseBackwardRequest.safeParse(body);
      if (!parsed.success) return { response: json(400, { error: parsed.error.message }) };
      const refused = resolveGrant(parsed.data.grantId);
      if (refused) return { response: refused };
      if (parsed.data.issue !== undefined && parsed.data.issue !== issue) {
        return {
          response: failure(
            400,
            "ISSUE_NOT_GRANTS",
            `a phase worker moves only its own issue ${issue}, not ${parsed.data.issue}`
          ),
        };
      }
      // The workflow (engine.go's `backward`): a move it refuses changes nothing; one it takes
      // moves the issue to the phase it names, counts the move on the implementer's row and
      // starts the role that works that phase, clearing its completion (`enter`), so that role's
      // next completion is of that phase. The move posts its sentence on the issue, so where the
      // label's workflow refuses a reason (`REASON_REFUSALS`) the sentence is read by Dispatch's
      // own reader through the label's own build, and a sentence Dispatch would refuse is refused
      // before anything moves (effects.go's `backwardReasonRefusal`).
      const { to, reason } = parsed.data;
      const from = served();
      const why = backwardRefusal(from, to);
      if (why !== undefined) return { response: failure(409, "BACKWARD_REFUSED", why) };
      const posted = POSTS_BACKWARD ? backwardMoveReason(from, to, reason) : "";
      let over: PostRefusal | undefined;
      try {
        over = REASON_REFUSALS ? postRefusal(CHECKOUT, posted) : undefined;
      } catch (error) {
        // The label's own reading did not answer: a rig that cannot read the reason never
        // answers the move as some other daemon would.
        return {
          response: json(500, {
            error: `the move's reason could not be read as the daemon reads it: ${error instanceof Error ? error.message : String(error)}`,
          }),
        };
      }
      if (over !== undefined) {
        const refusal = over.decision ? REASON_DECISION : REASON_TOO_LONG;
        return {
          response: failure(409, refusal.code, filled(refusal.format, over.sentence)),
        };
      }
      // The store of a label that takes a body holding U+0000 fails a move whose reason holds one,
      // since the task of the role the move starts carries the reason (`storeFails`): nothing
      // moves.
      if (storeFails(reason))
        return { response: failure(500, FACT_APPLY_FAILED.code, FACT_APPLY_FAILED.fact) };
      enter(to, "move", posted);
      return { response: json(200, {}) };
    }
    default:
      return { response: json(404, { error: "no route" }) };
  }
}

// The daemon's boot publishes the role holders of every issue with a live claim or a claim that
// changed lately (`queueRoleHoldersPublish`), which this issue is: the world's history and the
// agents before this one hold their roles on the run's Dispatch before this one's agent boots.
queueHolders();
// The run of the step the issue stands in, which the start of its phase opened before this boot:
// its start, which the world's seeds wrote as the phase before it ended (seed-handoffs.ts's
// `stepsSeed`) and Dispatch then keeps, since it writes each field of a record once, and which
// this writes where no seed did.
const opened = runsStep(PHASE) ? RUNS.get(PHASE) : undefined;
if (opened !== undefined)
  queueStep(opened.run, stepStart(opened.run, opened.startedAt ?? startedAt));
await runOutbox();

/** The daemon's refusal of a request body, `body` the JSON value it holds, a string of which, a
 * member's name or a value, holds U+0000, in the words `format` of the label's daemon
 * (`NUL_REFUSED`, one of daemon-pane.ts's `NUL_REFUSAL`), or undefined where none does: the first
 * such string in the order the body writes them, named by its place (`nulIn`), and where its first
 * U+0000 stands, counted from 1 in code points (storable_text.go's `unstorableJSON` and
 * `unstorableText`, claims.go's `refuseBadBody`). */
function nulRefusal(format: string, body: unknown): string | undefined {
  const found = nulIn(body, "");
  if (found === undefined) return undefined;
  const at = [...found.text.slice(0, found.text.indexOf("\0"))].length + 1;
  return `${NUL_REFUSAL.prefix}${filled(format, found.place, at)}`;
}
/** The first string of `value`, standing at `place` in its body, that holds U+0000, and its place
 * as Dispatch's walk names it, the one the daemon reads a body with (dispatchbody's `NulInJSON`):
 * `summary`, `capabilities.rows[0].detail`, a member's name with each U+0000 written as `\u0000`,
 * or `body` for a bare string. The value is the one `request.json()` gave, whose members keep the
 * order the body writes them in, but for a name that is an array index, which no body of the
 * `legion` tool holds. */
function nulIn(value: unknown, place: string): { place: string; text: string } | undefined {
  if (typeof value === "string")
    return value.includes("\0") ? { place: place === "" ? "body" : place, text: value } : undefined;
  if (Array.isArray(value)) {
    for (const [index, element] of value.entries()) {
      const found = nulIn(element, `${place}[${index}]`);
      if (found !== undefined) return found;
    }
    return undefined;
  }
  if (typeof value !== "object" || value === null) return undefined;
  for (const [name, member] of Object.entries(value)) {
    const named = name.replaceAll("\0", "\\u0000");
    const at = place === "" ? named : `${place}.${named}`;
    if (name.includes("\0")) return { place: at, text: name };
    const found = nulIn(member, at);
    if (found !== undefined) return found;
  }
  return undefined;
}

/** `GET /legion/v1/issues/<key>/record`, and the key it names. */
const ISSUE_RECORD_PATH = /^\/legion\/v1\/issues\/([^/]+)\/record$/;

/** The answer to one request of a route the label's daemon registers (`ROUTES`): the POST routes
 * `handle` serves, then the three GET routes, the state and the record for a caller stating
 * `stated` in `Legion-Plugin-Contract` (`unshaped`) and the health route, then the 404 every other
 * request gets, the catch-all's. */
async function answer(
  method: string,
  path: string,
  body: unknown,
  stated: string | null
): Promise<{ response: Response; mintedGrantId?: string; phase?: string }> {
  if (method === "POST") return handle(path, body);
  if (method === "GET") {
    if (path === "/healthz") return { response: json(200, HEALTH) };
    if (path === "/legion/v1/state") return { response: await stateAnswer(stated) };
    const record = ISSUE_RECORD_PATH.exec(path);
    if (record !== null) return { response: await recordAnswer(record[1] ?? "", stated) };
  }
  return { response: json(404, { error: "no route" }) };
}

const server = Bun.serve({
  hostname: "127.0.0.1",
  port: Number(portArg),
  async fetch(request) {
    const path = new URL(request.url).pathname;
    let body: unknown;
    if (request.method === "POST") {
      try {
        body = await request.json();
      } catch {
        body = undefined;
      }
    }
    // A route the label's daemon registers none of is answered as its catch-all answers it, 404
    // `{"error":"no route"}`, whatever case `handle` has for it (`ROUTES`). Every route it registers
    // decodes a POST's body first, and refuses one holding U+0000 there (`nulRefusal`).
    const nul =
      request.method === "POST" && NUL_REFUSED !== undefined
        ? nulRefusal(NUL_REFUSED, body)
        : undefined;
    const { response, mintedGrantId, phase } = !serves(ROUTES, request.method, path)
      ? { response: json(404, { error: "no route" }) }
      : nul !== undefined
        ? { response: json(400, { error: nul }) }
        : await answer(request.method, path, body, request.headers.get(PLUGIN_CONTRACT_HEADER));
    // A daemon that states its contract states it on every answer, set before any route answers
    // (server.go's `statingContract`): a refusal, the catch-all's 404 and a 204 as much as a
    // success. A daemon before it states none.
    if (DAEMON_CONTRACT !== undefined)
      response.headers.set(DAEMON_CONTRACT_HEADER, String(DAEMON_CONTRACT));
    // What the daemon last saw of the issue's tree move (`treeMovedAt`): a request of the
    // claim's that it took.
    if (request.method === "POST" && response.status < 300) movedAt = new Date().toISOString();
    // A move or a claim change this request made owes the run's Dispatch its rows, written before
    // the answer (`runOutbox`); a state read has written them before it built the state.
    if (request.method === "POST") await runOutbox();
    // A refusal's line carries its code, the daemon's `Failure` code where the answer has one, so
    // a reader of the log can tell the daemon's own answer to a run (HANDOFF_NOT_CURRENT_PHASE)
    // from a body this stand-in could not take at all, whose 400 has none (score.ts's
    // `unscored`). Every line carries the request's method and path, the route health holds a
    // 404 to (health.ts's `routes`). A completion's line carries the phase it was answered on,
    // which no program reads (completion-answers.ts's `DaemonLine`): it is how a person reading
    // the run tells a late completion from one of the role's own phase. A turn outcome's line
    // carries whether its answer failed the claim, which health reads (health.ts's `failedClaim`).
    const refusal: unknown = response.status >= 400 ? await response.clone().json() : undefined;
    const outcome: unknown =
      path === "/legion/v1/claims/turn-outcome" && response.status === 200
        ? await response.clone().json()
        : undefined;
    const line: LogLine = {
      at: new Date().toISOString(),
      method: request.method,
      path,
      status: response.status,
      role,
      ...carried("code", stringField(refusal, "code")),
      ...carried("grantId", stringField(body, "grantId")),
      ...carried("sessionId", stringField(body, "sessionId")),
      ...carried("mintedGrantId", mintedGrantId),
      ...carried("handoffId", stringField(body, "handoffId")),
      ...carried("handoffVersion", numberField(body, "handoffVersion")),
      ...carried("to", stringField(body, "to")),
      ...carried("reason", stringField(body, "reason")),
      ...carried("phase", phase),
      ...carried("failed", booleanField(outcome, "failed")),
    };
    await appendFile(logFile, `${JSON.stringify(line)}\n`);
    return response;
  },
});
// The record is opened here, with one line of its own and before any request can be answered,
// so a run whose agent never reached the daemon leaves a log holding that line and nothing
// else — the reading "no move" — while a log nothing wrote is absent and is the rig error it
// has always been (score.ts's `unscored`). It is written synchronously, between the socket
// opening and the first handler turn, so it is the record's first line whatever arrives next.
/** The record's own name, the one the score knows it by (score.ts's RECORDS). The line itself
 * is the one every record of a run is opened with (run-records.ts's `openingLine`). */
const record = logFile.slice(logFile.lastIndexOf("/") + 1);
appendFileSync(logFile, openingLine(record));
console.log(`listening on http://127.0.0.1:${server.port}`);
