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
 * each of its own daemon calls (`grants`), the issue's record (`GET /legion/v1/state`, the tool's
 * `read_record`), the phase completion (`handoff/complete`, the tool's `handoff_complete`) and
 * the return of the issue to an earlier phase (`phase/backward`, the tool's
 * `request_backward_move`), the two routes here that redeem a grant. No credential route and no
 * grant file: a pane's `gh` and
 * `git` read the rig's token from the gh files under its `GH_CONFIG_DIR`, which `daemon-pane.go`
 * writes, and no agent runs `legion` from bash. A route the label's daemon does not register is
 * answered 404 whatever this has a handler for, as that daemon's catch-all answers it; a route
 * it registers and this does not answer is answered 404 too, which `rig.sh health` reads as a
 * world the daemon would not have served (health.ts's `routes` rule).
 *
 * Every request and response body is the Go daemon's (`@legion/contracts/legion-api`): the
 * tool's grant request, its completion, and the state document. The grant rule mirrors the real
 * daemon's: a grant lives for 60 seconds and redeems any number of times while it lives; an
 * unknown or expired grant id answers 403 `{"error":"Invalid or expired grant"}`. Nothing is
 * single-use. A completion is answered as the run's label's daemon answers it
 * (completion-answers.ts's `LABEL_DAEMONS`, read by the label the run directory's name ends with,
 * `labelOf`), on the phase the issue stands in now (`served()`), never on the phase the role was
 * booted in. First the label's route (`packages/daemon/internal/api/handoff.go`, `RecordRoute`),
 * which decides whether the completion is record-backed. This checkout's (`left`): `of :=
 * phase.Of(grant.Role, issue.Phase); word, writer, handoffPhase := phase.Handoff(of);
 * recordBacked := handoffPhase && writer == grant.Role && (of == issue.Phase || namesRecord)`.
 * Main `ff2a10ef`'s (`issue`): `phase.Handoff(issue.Phase)` and `recordBacked := handoffPhase &&
 * writer == grant.Role`. A record-backed completion naming no record (`handoffId` and a positive
 * `handoffVersion`) is refused 400 HANDOFF_RECORD_REQUIRED, and one naming a record is read back
 * from Dispatch, which the stand-in takes as found (below); a completion naming one of a phase
 * that ends with none is refused 409 HANDOFF_RECORD_MISMATCH. Then the workflow
 * (`packages/daemon/internal/workflow/engine.go`, `handoff`): a completion from
 * the role whose phase the issue stands in is taken `{}` and moves nothing, since the record
 * moves on the review, as main's engine moves it (`servedPhase`). A completion from any other
 * role (a late role) is answered by the label's engine (`LateRole`):
 *   - this checkout's `leftItsPhase` takes it, `{}` and moving nothing, when the daemon started
 *     the role on the issue and the role's row holds no completion (`row.Completed()`). The
 *     completion is then one of the phase the role left (`phase.Of`), whose record it must name,
 *     or it is refused 409 HANDOFF_RECORD_REQUIRED (`phase.Handoff(reported)`, through
 *     `refused`). A role whose row holds a completion is refused 409 HANDOFF_NOT_CURRENT_PHASE.
 *     That holds for the roles the run's world says have completed (the [completed] argument:
 *     threads-closed's implementer, whose implementing round stands) and for a role whose
 *     completion was taken here, since the engine writes the completion's commit and record on
 *     the row (`row.HandoffCommit`, `row.HandoffRecord`). Every rig half is a worker the daemon
 *     started, so the refusal of a role the daemon never started is not modelled;
 *   - main `ff2a10ef` has no `leftItsPhase` and refuses every late completion 409
 *     HANDOFF_NOT_CURRENT_PHASE (`if RoleFor(issue.Phase) != fact.Role`).
 * The phase a completion is of (`phase.Of`) is the served phase when the role works it and
 * otherwise the role's own, its first (below): the implementer's would be retro once the issue
 * stands in merging, where no run of this rig stands. The route's entry fence (`entryFence`:
 * HANDOFF_STALE_ENTRY, or HANDOFF_NOT_CURRENT_PHASE in another role's phase, for a completion of
 * a phase entry its role's row has moved past, decided by `behindEntryClause` on the head the
 * record names) is not modelled: it compares two entries, the one the claim serves and the one
 * the role's row names, and those differ only once the issue has entered the role's phase again
 * while its worker was on the earlier entry. No run of this rig enters a phase again: the issue
 * leaves `reviewing` at most once and stays where it moved (`servedPhase`), and every other
 * phase stands still, so a rig half's completion is always of the entry its row names.
 * The NOT_CURRENT_PHASE code and message are the daemon's own, read from its code at boot
 * (`NOT_CURRENT_PHASE`), because the skill has the worker quote them to its architect. The real
 * daemon's earlier fence, the 409 `HANDOFF_NO_RUN` of a completion it can attribute to
 * no run (`internal/api/handoff.go`, `noRunCompletion`), is not the one a rig half meets: both
 * halves are live registered workers serving generation 1, so the route finds a run and the
 * refusal is the workflow's. A role's phase is its first (daemon-pane.go's `phase <role>`),
 * which is narrower than the daemon, whose implementer also runs retro; no run of this rig
 * stands in retro. The body may carry every key any
 * label's contract sends (`CompleteRequest`). A backward move answers `{}` once its
 * grant redeems, as the daemon's does for a phase worker, and names no issue but this one
 * (`ISSUE_NOT_GRANTS`, 400, the daemon's refusal of a worker that moves another issue). The
 * stand-in reads no record from
 * Dispatch: the `legion` tool found the newest record of the phase on Dispatch before posting, and
 * the rig's scorer reads that record back from Dispatch and holds it to the run's session
 * (score.ts testerProof).
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
 * <run dir>/built-from names (`ROUTES`).
 * The state document holds that one issue, on the repository and default branch of the run's own
 * world, which it reads from <run dir>/world.json, as the gh stand-in does: a record naming
 * another repository sends the agent's review to one the world has not got, and a record naming
 * another branch sends its work onto a branch the world has not got (rig.sh's worker world
 * comment). The issue stands in [phase], or,
 * where the caller names none, in the first phase its role works (daemon-pane.go's `phase
 * <role>`): the implementer works implementing and retro, and the daemon starts it in either.
 * [completed] is the roles, comma-separated, whose row the run's world gives a handoff commit:
 * each completed its phase before the run began (`COMPLETED_ROWS`).
 * An issue in `reviewing` moves on what the same run directory reports: its remote's tip, the
 * reviews of its world and of its gh record, and the merge state its world reports for that tip
 * (`servedPhase`).
 * Prints `listening on http://127.0.0.1:<port>` once the socket is open (port 0: the kernel's
 * pick).
 */
import { randomUUID } from "node:crypto";
import { appendFileSync, existsSync, readFileSync } from "node:fs";
import { appendFile } from "node:fs/promises";
import { isLegionRole, roleToken } from "@legion/contracts";
import {
  LegionGrantRequest,
  LegionHandoffCompleteRequest,
  LegionPhaseBackwardRequest,
  LegionReleaseRecordsRequest,
  LegionStateResponse,
  LegionTurnOutcomeRequest,
} from "@legion/contracts/legion-api";
import { z } from "zod";
import {
  COMPLETE_PATH,
  LABEL_DAEMONS,
  labelOf,
  NOT_CURRENT_PHASE as NOT_CURRENT_PHASE_CODE,
} from "./completion-answers";
import {
  claimRefusal,
  countDefault,
  labelCheckout,
  labelPhase,
  labelRoutes,
  notCurrentPhase,
  serves,
} from "./daemon-pane";
import { mergeStateOn, routeFor, worldOf } from "./gh-call";
import { CALLS, openingLine } from "./run-records";

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
/** How the run's label's daemon answers a late role and which phase its route holds a record to
 * (completion-answers.ts's `LateRole` and `RecordRoute`), read by the label the run directory's
 * name ends with. A run of a label the rig models no daemon for fails the boot rather than being
 * answered as some other label's daemon would answer it. */
const { lateRole: LATE_ROLE, recordRoute: RECORD_ROUTE } = (() => {
  try {
    const label = labelOf(runDir);
    const daemon = LABEL_DAEMONS[label];
    if (daemon === undefined) throw new Error(`no daemon is declared for the ${label} label`);
    return daemon;
  } catch (error) {
    console.error(`daemon-standin: ${error instanceof Error ? error.message : String(error)}`);
    process.exit(2);
  }
})();
/** The roles whose phase row holds a completion, which this checkout's engine refuses a late
 * completion from (`leftItsPhase`'s `row.Completed()`, a handoff commit or a handoff record on
 * the row): the roles the run's world says completed their phase with a commit before the run
 * began, and a role whose completion was taken here naming a commit or a record (`handle`'s
 * `handoff/complete`). */
const COMPLETED_ROWS = new Set(completedArg.split(",").filter((named) => named !== ""));
for (const named of COMPLETED_ROWS) {
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
const { repo, base, branch: BRANCH, pr: PR } = worldOf(runDir, "daemon-standin");
/** The owner of the run's repository, which is the one owner the project maps. */
const [REPO_OWNER = ""] = repo.split("/");
if (REPO_OWNER === "" || !repo.includes("/")) {
  console.error(`${repo} is not an <owner>/<name> repository`);
  process.exit(2);
}
/** Encoded exactly as the daemon encodes it (`legion-<project>-<key>-<role>`, lower-cased),
 * since the worker claims this token on the real Envoy and Envoy rejects uppercase. */
const ROLE_TOKEN = roleToken(project, issue, role);
const SESSION_SECRET = "rig-secret";
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
 * `labelCheckout`), whose daemon every fact below is read out of: its routes, its phase table,
 * its refusals' sentences and its launch failure limit. Each is read through daemon-pane.ts's
 * facts cache, so a boot whose sources a boot before it read runs no `go run`. */
const CHECKOUT = bootRead(() => labelCheckout(runDir));
/** The routes the run's label's daemon registers (daemon-pane.ts's `labelRoutes`): the one answer
 * to "does this daemon serve that route", which `fetch` asks of every request and health asks of
 * every 404 the log holds. */
const ROUTES = bootRead(() => labelRoutes(CHECKOUT));
/** The run of refused turns at which the daemon fails the claim: the launch failure limit
 * (supervise's `turnOutcome`, `run < m.deps.Limits.LaunchFailures`), which the daemon is given as
 * `launch_failure_limit` and the rig, having no legion.yaml, runs at its default, read out of the
 * label's config source (daemon-pane.ts's `countDefault`). Read only for a daemon that serves
 * `claims/turn-outcome`. */
const TURN_OUTCOME_LIMIT = serves(ROUTES, "POST", "/legion/v1/claims/turn-outcome")
  ? bootRead(() => countDefault(CHECKOUT, "launch_failure_limit"))
  : 0;
/** The claim refusals the claim routes answer a caller with (packages/daemon/internal/claim's
 * `InvalidSecret`, `InvalidBootToken` and `StaleGeneration`), each its status and its sentence,
 * read out of the label's claim source (daemon-pane.ts's `claimRefusal`). */
const INVALID_SECRET = bootRead(() => claimRefusal(CHECKOUT, "InvalidSecret"));
const INVALID_BOOT_TOKEN = bootRead(() => claimRefusal(CHECKOUT, "InvalidBootToken"));
const STALE_GENERATION = bootRead(() => claimRefusal(CHECKOUT, "StaleGeneration"));
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

const startedAt = new Date().toISOString();
/** The phase the role's own worker is started in, the first of the transition table it works,
 * and the word of the handoff record that phase ends with, empty when it ends with none, as the
 * label's daemon has them (daemon-pane.ts's `labelPhase`). */
const { phase: ROLE_PHASE, word: HANDOFF_WORD } = bootRead(() => labelPhase(CHECKOUT, role));
/** The phases that end with a handoff record (`phase.Handoff` in
 * packages/daemon/internal/phase/phase.go): the route refuses a completion naming a record in any
 * other phase. daemon-pane.go answers only the role's own phase, at a `go run` each, so the rest
 * is written here, and the boot fails when the daemon's answer for the role's own phase disagrees
 * with it. */
const RECORD_PHASES = ["planning", "implementing", "testing", "reviewing"];
if ((HANDOFF_WORD !== "") !== RECORD_PHASES.includes(ROLE_PHASE)) {
  console.error(
    `daemon-standin: the daemon gives ${ROLE_PHASE} the handoff word '${HANDOFF_WORD}', which RECORD_PHASES does not agree with`
  );
  process.exit(2);
}
/** The phase the issue stands in for this run: the caller's, or the role's own when it names
 * none. A caller that names another is a run of a loop the daemon starts no phase for, where
 * the role's completion is a late one (`handle`'s `handoff/complete`). */
const PHASE = phaseArg === "" ? ROLE_PHASE : phaseArg;
/** Stands in for the issue's phase in the refusal read at boot, and is replaced with the phase
 * served when the refusal is sent. */
const PHASE_SLOT = "PHASESLOTOFTHEISSUE";
/** The daemon's own refusal of a late completion of this role, its code and its message, read
 * from the label's daemon's code at boot (daemon-pane.ts's `notCurrentPhase`) rather than written
 * here: the skill has the worker quote that sentence to its architect. It is read once, with the
 * phase left as `PHASE_SLOT`, since the phase the issue stands in when the refusal is sent is the
 * one served then. It is read only for a run whose role can be late: one booted in a phase that
 * is not the role's, or in `reviewing`, the one phase the review round moves. A daemon whose
 * refusal cannot be read fails the boot rather than the completion. */
const NOT_CURRENT_PHASE = (() => {
  if (PHASE === ROLE_PHASE && PHASE !== "reviewing") return undefined;
  const refusal = bootRead(() => notCurrentPhase(CHECKOUT, role, PHASE_SLOT, issue));
  if (refusal === undefined || refusal.code !== NOT_CURRENT_PHASE_CODE) {
    console.error(
      `daemon-standin: daemon-pane printed no ${NOT_CURRENT_PHASE_CODE} code and message for the refusal of ${role}'s late completion`
    );
    process.exit(2);
  }
  return refusal;
})();
/** `GET /legion/v1/state`: the one issue the worker holds, standing in the phase this stand-in
 * was started for, on the run's own world: its repository and the branch its pull request is
 * open against, both read from that world and written nowhere else here. A record that named
 * one repository while the run's gh stand-in served another had agents query both, the second
 * answering 404, and cost recorded runs their review: rig.sh's worker world comment holds that
 * story. `capabilities` is the
 * deployment's capability report, `strandedClaims` the claims nothing will wake, `heldOutboxRows`
 * the lifecycle rows held behind their issues' running ones, `consumers` the lag of the daemon's
 * durable consumers and `repositories` the project's mapping, each of which the reader requires;
 * the rig has no deployment to report on, its one claim runs, it has no outbox and no NATS, so
 * the first four are the empty lists the daemon's MarshalJSON emits when there is none.
 * `wakesWaiting`, the wakes the pacing holds until their turn, is optional to the reader; the rig
 * carries it empty, as a daemon at this contract serves a caller in no pod holding no wake.
 * `settings` is the daemon's resolved legion.yaml with no credential in it, also required; the rig
 * has no file, so it carries the two keys the reader types at the daemon's defaults. */
const stateIn = (phase: string) =>
  LegionStateResponse.parse({
    daemon: { project, schemaVersion: 1, boots: 1, firstBootAt: startedAt, startedAt },
    admission: { cap: 1, active: [issue], waiting: [], trees: [issue], blocked: [] },
    issues: {
      [issue]: {
        key: issue,
        generation: 1,
        phase,
        status: "in_progress",
        tree: issue,
        repo,
        defaultBranch: base,
        workers: {},
      },
    },
    pendingStatusWrites: [],
    heldOutboxRows: [],
    strandedClaims: [],
    wakesWaiting: [],
    capabilities: [],
    repositories: {
      owners: [REPO_OWNER],
      mapped: [{ repo, default: true }],
      refused: [],
      withoutWebhookApp: [],
      caches: [],
    },
    // Every key any label's contract requires, not this checkout's alone: a label's agents load
    // the plugin of their own checkout, and `legionSettings` is a loose object in each, so a body
    // carrying both keys is described by both contracts while one carrying either alone is
    // refused by the other. Measured on 2026-11-10: the base label, built from main `ff2a10ef`,
    // whose contract requires `settings.max_fix_attempts`, had every state read of all 30 of its
    // runs refused `a body the daemon API contract does not describe`, so its agents spent turns
    // on the daemon and no reviewer of that label could reach the `read_record` the rule asks
    // for — the negative control of k1 and k2 measured in a world of its own. A key a later
    // contract drops stays here for as long as a label the rig builds still requires it.
    settings: { max_fix_attempts: 3, review_round_cap: 3, worker_boot_timeout_seconds: 120 },
    consumers: [],
  });

/** The phase the stand-in serves now, which starts as the phase it was started for and moves as
 * main's engine moves it: `reviewRound` (`packages/daemon/internal/workflow/review.go`), and the
 * transitions its outcomes take (`workflow/table.go`). Only `reviewing` moves: the rig's run ends
 * with any other phase's completion, and no scenario reads the record after it.
 *
 * The review decides the round and the completion does not, so this moves on what the run's
 * world reports and never on a completion (`roundOutcome`, read at each state read): the head is
 * the tip of the run's own remote, the decision is the last `APPROVED` or `CHANGES_REQUESTED`
 * among the reviews the world held when the run began and those the run left, and GitHub's merge
 * state for the head is the world's (gh-call.ts's `mergeStateOn`). It does what `reviewRound`
 * does with them:
 *   - a change request ends the round rejected whatever head it names and whether or not the
 *     reviewer has completed, and the issue goes back to `implementing` (`TriggerReviewRejected`).
 *     reviewer-text-findings' defect case reaches it, in `reviewing`;
 *   - an approval of the head (`classify.ApprovalStands`) ends the round approved when GitHub
 *     reports the head `CLEAN`, `UNSTABLE` or `HAS_HOOKS` (`record.MergeState.Mergeable`), and the
 *     issue moves to `accepting` (`TriggerReviewApproved`), on the approval alone
 *     (`review_test.go`'s "after the review alone"). Any other state — `BLOCKED`, which a check
 *     the base requires holds the head in while it runs, `BEHIND`, `DIRTY`, `UNKNOWN` — leaves
 *     the issue in `reviewing`, where the engine waits on GitHub's answer or reports the round
 *     stuck, and neither moves the record;
 *   - an approval of another head is set aside (`classify.ApprovalSetAside`, the round
 *     `stuckApprovedOtherCode`), which is what a push while the record is `reviewing` leaves, and
 *     the issue stays in `reviewing` until a review of the new head decides the round.
 * Once the record has moved it stays where it moved to: a push in `accepting` lands outside any
 * round (`review.go`'s `issue.Phase != phase.Reviewing`, `review_test.go`'s "the approval of the
 * head, then a push"). A reviewer's completion after its decision is a late one, answered as the
 * run's label's engine answers it (`handle`'s `handoff/complete`).
 *
 * The check runs of a head are no part of the move, as they are none of `reviewRound`'s but for
 * the report of a stuck round (`classify.ChecksConcludedOnHead`), which moves nothing: they are
 * what an agent reads (gh-standin.ts's `headRuns`). Not modelled: a push on its way
 * (`classify.CodeOnItsWay`), since the run's remote takes each push whole and its tip is never a
 * head the pull request is still waiting for. */
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
    })
  ),
  mergeStates: z.unknown().optional(),
});
/** A review as the world's reviews route lists it, and one line of the gh stand-in's record,
 * which carries the review that call left as that stand-in answered it (gh-standin.ts's
 * `answered`). */
const ListedReview = z.looseObject({ state: z.string(), commit_id: z.string() });
const RecordedCall = z.looseObject({
  review: z.object({ state: z.string(), head: z.string() }).optional(),
});
/** The states a review decides a round with (`classify`'s `DecidesRound`): a COMMENT and a
 * pending review decide nothing, and leave the decision before them standing. */
const DECIDES = ["APPROVED", "CHANGES_REQUESTED"];
/** The merge states the issue may advance on (`record.MergeState.Mergeable`). */
const MERGEABLE = ["CLEAN", "UNSTABLE", "HAS_HOOKS"];

/** The phase the review round moves the issue to on what the run's world reports now, or
 * undefined while it stays in `reviewing` (`servedPhase`). */
function roundOutcome(): "accepting" | "implementing" | undefined {
  const tip = Bun.spawnSync([
    "git",
    "-C",
    `${runDir}/remote.git`,
    "rev-parse",
    `refs/heads/${BRANCH}`,
  ]);
  if (tip.exitCode !== 0 || !existsSync(`${runDir}/fixtures.json`)) return undefined;
  const head = tip.stdout.toString().trim();
  const { routes, mergeStates } = Fixtures.parse(
    JSON.parse(readFileSync(`${runDir}/fixtures.json`, "utf8"))
  );
  const listed = routeFor(routes, ["api", `repos/${repo}/pulls/${PR}/reviews`])?.stdout;
  const recorded = existsSync(`${runDir}/${CALLS}`)
    ? readFileSync(`${runDir}/${CALLS}`, "utf8")
        .split("\n")
        .filter((line) => line.trim() !== "")
        .flatMap((line) => {
          const review = RecordedCall.safeParse(JSON.parse(line)).data?.review;
          return review === undefined ? [] : [review];
        })
    : [];
  const decision = [
    ...(Array.isArray(listed) ? listed : []).flatMap((review: unknown) => {
      const read = ListedReview.safeParse(review);
      return read.success ? [{ state: read.data.state, head: read.data.commit_id }] : [];
    }),
    ...recorded,
  ].findLast((review) => DECIDES.includes(review.state));
  if (decision?.state === "CHANGES_REQUESTED") return "implementing";
  const stands =
    decision !== undefined && decision.head.length >= 7 && head.startsWith(decision.head);
  return stands && MERGEABLE.includes(mergeStateOn(mergeStates, head)) ? "accepting" : undefined;
}

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

const grants = new Map<string, { readonly expiresAt: number }>();

/** `POST /legion/v1/handoff/complete`'s body: every key any label's contract sends, not this
 * checkout's alone, for the reason the state body carries every settings key (`stateIn`): a
 * label's agents load the plugin of their own checkout. The base label, built from main
 * `ff2a10ef`, sends `ready`, a boolean its contract requires on every completion; #2539 took it
 * out of this checkout's request, a strict object, so a stand-in parsing with this checkout's
 * alone answered every base completion 400 with zod's `unrecognized_keys`. Measured on
 * 2026-11-12: all 20 base runs of k1 and k2 logged `handoff/complete` 400 and nothing else, and
 * each scored a world of its own (review 5887659447, finding 1). The object stays strict, so a
 * key no label sends is still refused, and the daemon of that label answered a completion
 * carrying `ready` 200, as this does. A key a later contract drops stays here for as long as a
 * label the rig builds still sends it. */
const CompleteRequest = LegionHandoffCompleteRequest.extend({ ready: z.boolean().optional() });

/** The phase a state read serves now: `servedPhase`, moved where the round sends the issue the
 * first time a read finds the round over while the record is `reviewing` (`roundOutcome`). */
function served(): string {
  if (servedPhase === "reviewing") servedPhase = roundOutcome() ?? servedPhase;
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
 * claim. Only the Claude bridge reports under a boot token, no rig agent runs one, and a run
 * whose claim failed is unhealthy whatever comes after (health.ts's `failedClaim`), so this
 * answers it with its own sentence and names no daemon's. */
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
 * served, so the run is unhealthy and never scored. */
function handle(
  path: string,
  body: unknown
): { response: Response; mintedGrantId?: string; phase?: string } {
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
      grants.set(grantId, { expiresAt });
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
      // Both the route and the workflow read the phase the issue stands in now, the one a state
      // read serves, and never the phase this role was booted in. The earlier HANDOFF_NO_RUN
      // fence of the route is not reached: a rig half is a live registered worker serving
      // generation 1, so the route attributes its completion to a run; nor is the entry fence
      // (the module doc says why).
      const phase = served();
      const { handoffId, handoffVersion, commit } = parsed.data;
      const namesRecord = handoffId !== undefined || handoffVersion !== undefined;
      // The route (packages/daemon/internal/api/handoff.go), as the label's daemon has it
      // (`RECORD_ROUTE`). A phase's writer is the role the workflow starts on it. This
      // checkout's holds the completion to the phase it is of (`phase.Of`): the served phase
      // when this role works it, and otherwise the one it left, its own; the completion is
      // record-backed when that phase ends with a record and it is the served phase or the
      // completion names a record. `ff2a10ef`'s holds it to the served phase, and it is
      // record-backed exactly when the issue stands in this role's own phase and that phase ends
      // with a record.
      const of = RECORD_ROUTE === "left" ? ROLE_PHASE : phase;
      const recordBacked =
        RECORD_ROUTE === "left"
          ? HANDOFF_WORD !== "" && (phase === ROLE_PHASE || namesRecord)
          : phase === ROLE_PHASE && HANDOFF_WORD !== "";
      if (recordBacked) {
        // A record it names is read back from Dispatch and held to the issue, the word, the
        // version and the session (`handoffRecord`), which the stand-in takes as found: the
        // scorer reads the record back instead (the module doc).
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
      // The workflow (packages/daemon/internal/workflow/engine.go, `handoff`): a late role's
      // completion is answered by the label's engine (LATE_ROLE). The refusal's code and message
      // are the daemon's own, read from its code at boot (NOT_CURRENT_PHASE), because the skill
      // has the worker quote them to its architect.
      if (phase !== ROLE_PHASE) {
        if (LATE_ROLE === "refused" || COMPLETED_ROWS.has(role)) {
          if (NOT_CURRENT_PHASE === undefined)
            throw new Error(`no refusal was read at boot for ${role}'s completion in ${phase}`);
          return {
            phase,
            response: failure(
              409,
              NOT_CURRENT_PHASE.code,
              NOT_CURRENT_PHASE.message.replaceAll(PHASE_SLOT, phase)
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
      // A taken completion is written on the role's row, its commit as the row's handoff commit
      // and its record as the row's handoff record, either of which completes the row
      // (`row.Completed()`), and it moves nothing: the review decides the round, and the record
      // moves when the world reports a decision that ends it (`servedPhase`, `roundOutcome`).
      if (commit !== undefined || handoffId !== undefined) COMPLETED_ROWS.add(role);
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
      return { response: json(200, {}) };
    }
    default:
      return { response: json(404, { error: `no route for ${path}` }) };
  }
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
    // A route the label's daemon registers none of is answered as its catch-all answers it, 404,
    // whatever case `handle` has for it (`ROUTES`).
    const { response, mintedGrantId, phase } = !serves(ROUTES, request.method, path)
      ? { response: json(404, { error: `no route for ${request.method} ${path}` }) }
      : request.method === "POST"
        ? handle(path, body)
        : request.method === "GET" && path === "/legion/v1/state"
          ? { response: json(200, stateIn(served())) }
          : { response: json(404, { error: `no route for ${request.method} ${path}` }) };
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
