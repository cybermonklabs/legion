import { mayHaveReachedServer, outage, retryAfterMs } from "@legion/contracts/http-status";
import {
  type LegionActivityRequest,
  LegionAgentActionResponse,
  LegionChildRequest,
  LegionClaimRequest,
  LegionCompactResponse,
  LegionControllerGrantRequest,
  LegionControllerRegisterResponse,
  type LegionControllerRegistration,
  LegionDefectReportRequest,
  LegionDefectReportResponse,
  LegionDefectWithdrawRequest,
  LegionDefectWithdrawResponse,
  LegionEmptyResponse,
  LegionErrorResponse,
  LegionFreshSessionResponse,
  LegionGateRegisterRequest,
  LegionGrantRequest,
  LegionGrantResponse,
  LegionHandoffCompleteRequest,
  LegionHandoffCompleteResponse,
  type LegionHealth,
  LegionHealthResponse,
  LegionIntakeDiscardRequest,
  LegionIntakeDiscardResponse,
  LegionIntakeReplayRequest,
  LegionIntakeReplayResponse,
  type LegionIssue,
  LegionIssueRecordResponse,
  LegionIssueStatusRequest,
  LegionP0BoundResponse,
  LegionPhaseBackwardRequest,
  LegionPhaseRetryRequest,
  LegionPlanRegisterRequest,
  LegionPlanRegisterResponse,
  LegionPodUpdateRequest,
  LegionPodUpdateResponse,
  LegionProjectP0BoundClearRequest,
  LegionProjectP0BoundRequest,
  LegionProjectPauseRequest,
  LegionProjectPauseResponse,
  LegionProjectResumeRequest,
  LegionProjectResumeResponse,
  type LegionReadyForUpdateRequest,
  type LegionReadyRequest,
  LegionRegisterResponse,
  type LegionRegistration,
  LegionReleaseRecordsRequest,
  LegionReleaseRecordsResponse,
  LegionRootCloseRequest,
  LegionSignOffRequest,
  type LegionState,
  LegionStateResponse,
  LegionTaskStopRequest,
  LegionTurnOutcomeRequest,
  LegionTurnOutcomeResponse,
  LegionWaveReleaseRequest,
  LegionWaveReleaseResponse,
} from "@legion/contracts/legion-api";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import { messageFor } from "@legion/envoy-client/errors";
import type { z } from "zod";

/**
 * The plugin's client for the daemon (`packages/daemon`), the one `extensions/legion.ts` boots
 * every Legion session through.
 *
 * Every response is read through the answer schemas of `@legion/contracts/legion-api`, which
 * ignore a key they do not name: a daemon one contract ahead of this build is read rather than
 * refused, and the key it added is dropped from the value handed on. A missing field or a field
 * of a type the schema does not describe is still a refusal, and the daemon's golden tests still
 * hold the reader to the daemon's own types, by the equality between each golden's parse and its
 * file in `packages/contracts/src/legion-api.test.ts`. The requests are the claim wire of
 * `packages/daemon/internal/claim/wire.go`. The daemon refuses a member a route does not read
 * (`readBody`), except on the registration, whose body it reads leniently (`readRegisterBody`).
 * There is no secret recovery: the daemon persists a registration's capability before it answers,
 * so a restart forgets no secret.
 */

/** `claim.RegisterRequest`: the pane's boot token and the session this agent became.
 * `pluginContract` is this build's `legion.daemonApiVersion`. */
export interface RegisterInput {
  readonly bootToken: string;
  readonly sessionId: string;
  readonly ompSessionFile: string;
  readonly agentId: string;
  readonly pluginContract: number;
}

/** `claim.ReadyRequest`: the claim the registration issued, authenticated by its secret, and the
 * session's report of the six live capability rows (`src/capability-report.ts`, contract 21); the
 * controller's ready carries none. */
export type ReadyInput = z.input<typeof LegionReadyRequest>;

/** `claim.ReadyForUpdateRequest`: the worker's answer to its update round's message, saying it
 * has pushed everything it wants kept and the process holding this claim may be replaced. `round`
 * is the round the update message named, which the daemon takes the answer for and no other.
 * `head` is the commit it reports as the head of `legion/<issue>@origin`, left out when the branch
 * is not there. */
export type ReadyForUpdateInput = z.input<typeof LegionReadyForUpdateRequest>;

/** `claim.ExitRequest`: the agent reporting its own end, and why. */
export interface ExitInput {
  readonly claimToken: string;
  readonly sessionId: string;
  readonly secret: string;
  readonly generation: number;
  readonly reason: string;
}

/** `claim.ActivityRequest`: what a live role's session is doing, which the daemon writes onto its
 * claim and the idle rule reads (`src/session-activity.ts` builds it). */
export type ActivityInput = z.input<typeof LegionActivityRequest>;

export interface LegionDaemonClient {
  /** The daemon's whole state. `signal` aborts the request, its answer's body included: a caller
   * that stops waiting for a daemon that does not answer (the phase reminder's bounded record
   * check) aborts it rather than leave it open. */
  readonly state: (options?: { readonly signal?: AbortSignal }) => Promise<LegionState>;
  /** One issue's record, the value the whole state holds under `issues.<key>`, or `undefined`
   * for an issue the daemon records nothing of, which it answers 404 `RECORD_NOT_FOUND`. Every
   * other refusal throws, so a caller's `undefined` is the absent record and its `catch` is the
   * read that failed. `signal` aborts the request as `state`'s does. */
  readonly record: (
    issue: string,
    options?: { readonly signal?: AbortSignal }
  ) => Promise<LegionIssue | undefined>;
  /** That the daemon is serving, and the project it serves: one word for the controller's boot
   * check, which read the whole state for it before. */
  readonly health: (options?: { readonly signal?: AbortSignal }) => Promise<LegionHealth>;
  /** The contract the daemon states on its answer, or `undefined` where it states none. It
   * reads the health route's header and parses no body, so it answers for a daemon whose health
   * body this build cannot read: the two numbers are what a reader compares. */
  readonly daemonContract: (options?: {
    readonly signal?: AbortSignal;
  }) => Promise<number | undefined>;
  readonly register: (input: RegisterInput) => Promise<LegionRegistration>;
  /** The same route, for the operator-launched controller: `bootToken` is the capability `legion
   * controller start` fetched, and the answer is the controller's registration. */
  readonly registerController: (input: RegisterInput) => Promise<LegionControllerRegistration>;
  /** The agent saying it can be prompted. `signal` aborts the request, its answer's body
   * included: a caller that stops waiting for a daemon that does not answer (the capability
   * schedule's bounded send) aborts it rather than leave it open. */
  readonly ready: (input: ReadyInput, options?: { readonly signal?: AbortSignal }) => Promise<void>;
  readonly readyForUpdate: (input: ReadyForUpdateInput) => Promise<void>;
  readonly exit: (input: ExitInput) => Promise<void>;
  /** `claim.TurnOutcomeRequest`: how one of the claim's turns ended, under the sequence the
   * reporting plugin counted itself (`packages/pi-legion/src/turn-outcome.ts`). The answer is the
   * trailing run of refused turns the daemon derived (`LegionTurnOutcomeResponse` says what its
   * `failed` holds). */
  readonly turnOutcome: (
    input: z.input<typeof LegionTurnOutcomeRequest>
  ) => Promise<z.output<typeof LegionTurnOutcomeResponse>>;
  /** The session's report of what it is doing, after each of its turns and whenever the report's
   * content changes. It is authenticated by the claim, as `ready` is, and answers 204. */
  readonly activity: (input: ActivityInput) => Promise<void>;
  /** The deployment's idle-release records of one window: every release, resume and hold the
   * daemon recorded at or after `since`, oldest first. It is authenticated by the claim, as
   * `activity` is, and the claim's secret rides in the body rather than a query string, which
   * would carry it into every proxy and access log between the pod and the daemon. */
  readonly releaseRecords: (
    input: z.input<typeof LegionReleaseRecordsRequest>
  ) => Promise<z.output<typeof LegionReleaseRecordsResponse>>;
  readonly grant: (
    input: z.input<typeof LegionGrantRequest>
  ) => Promise<z.output<typeof LegionGrantResponse>>;
  readonly controllerGrant: (
    input: z.input<typeof LegionControllerGrantRequest>
  ) => Promise<z.output<typeof LegionGrantResponse>>;
  readonly handoffComplete: (
    input: z.input<typeof LegionHandoffCompleteRequest>
  ) => Promise<z.output<typeof LegionHandoffCompleteResponse>>;
  readonly issueStatus: (
    input: z.input<typeof LegionIssueStatusRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  /** The controller's `fresh_session`: the agent on `role` of `issue` leaves its recorded session
   * behind and starts again in a new one, the effect
   * `legion claims retry --fresh-session` runs for an operator. The answer is empty wherever
   * the agent came back and its start ran. `phaseNotRestarted` carries the workflow's refusal
   * of the phase restart, where the agent came back owing no task, or the machine's refusal of
   * a step that would have started the agent again, where this request stopped it. That
   * sentence names the state its claim stands in: read it in `read_state`, and ask for a fresh
   * session again only where the claim is suspended. */
  readonly freshSession: (
    input: z.input<typeof LegionClaimRequest>
  ) => Promise<z.output<typeof LegionFreshSessionResponse>>;
  /** The root architect's `register_gate`: its document for a design-gate step at a version and,
   * on a basis, the ancestor's document id (`basisArtifactId`), its `basisVersion` and the
   * architect's `reason` (entry 20261125161152 (gate-basis)), which the daemon takes together or not at all. */
  readonly gateRegister: (
    input: z.input<typeof LegionGateRegisterRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  /** The controller's `compact`: the agent on `role` of `issue` has its Oh My Pi session
   * compacted and is then sent the task it owes again, the effect `legion claims compact` runs
   * for an operator. The answer says whether the session was compacted and, when it was not, why;
   * the agent came back and whatever task it owes went again either way. */
  readonly compact: (
    input: z.input<typeof LegionClaimRequest>
  ) => Promise<z.output<typeof LegionCompactResponse>>;
  /** The controller's `stop`: the agent on `role` of `issue` is stopped and
   * retired, the effect `legion claims stop` runs for an operator; a turn it is in is cut off with
   * it. Refused for an agent the operator's own suspension holds (`SUSPENDED_BY_OPERATOR`). The
   * answer is the claim's state after the stop. */
  readonly stopAgent: (
    input: z.input<typeof LegionClaimRequest>
  ) => Promise<z.output<typeof LegionAgentActionResponse>>;
  /** The controller's `retry`: an agent that failed, or that a stop retired,
   * starts again in the session its claim recorded, the effect a plain `legion claims retry` runs
   * for an operator; a phase worker is started through its issue's phase. Refused for an agent
   * whose claim is stored working (`CLAIM_IN_A_TURN`), for an agent the operator's own suspension
   * holds (`SUSPENDED_BY_OPERATOR`), and for every agent while the project is paused
   * (`PROJECT_PAUSED`, with the pause that stands; its resume lifts that refusal). The answer is
   * the claim's state after the retry. */
  readonly retryAgent: (
    input: z.input<typeof LegionClaimRequest>
  ) => Promise<z.output<typeof LegionAgentActionResponse>>;
  /** The controller's `suspend`: the agent's process stops and its session is kept, the effect
   * `legion claims suspend` runs for an operator, held for a turn's end; the suspension is the
   * controller's own (`controller`), which holds against the daemon's rules and which its own
   * `resume_agent` lifts. Refused for an agent the operator's own suspension holds
   * (`SUSPENDED_BY_OPERATOR`). The answer is the claim's state after the suspension, with who
   * holds the suspension. */
  readonly suspendAgent: (
    input: z.input<typeof LegionClaimRequest>
  ) => Promise<z.output<typeof LegionAgentActionResponse>>;
  /** The controller's `resume_agent`: a suspended agent's session is relaunched, the effect
   * `legion claims resume` runs for an operator, but never over the operator's own suspension
   * (`SUSPENDED_BY_OPERATOR`); an agent whose claim is stored working is refused
   * (`CLAIM_IN_A_TURN`), and so is every agent while the project is paused (`PROJECT_PAUSED`): an
   * agent the pause stopped is the project's resume's to bring back. A stop and a suspension still
   * run while the project is paused. The answer is the claim's state after the resume. */
  readonly resumeAgent: (
    input: z.input<typeof LegionClaimRequest>
  ) => Promise<z.output<typeof LegionAgentActionResponse>>;
  /** The controller's `replace_pod`: the daemon opens its update round over the pod `issue` runs
   * in, as a drift opens one, and over a pod an earlier round kept as well. Each role of the pod is
   * asked to push its work and answer `ready_for_update`, and the round reaches its end once each
   * has answered, no turn runs and each role's report since its last turn names no running
   * background work, or at the round's bound. The daemon then reads the pod's workspace and
   * replaces the pod only where that read finds nothing that no branch or tag on origin holds;
   * where it finds such work, or fails, the round keeps the pod and ends with no replacement, and
   * the open phase's worker or the issue's architect is told. So a round that ended did not always
   * replace the pod: the pod uid on the issue's claims (`locator.sandbox.podUid`) says which.
   * The answer is the round, its reasons, and whether this request opened it, false for a round
   * already open over the pod. Refused for an issue of another project or with no claim the
   * daemon supervises (`NO_CLAIM`), one whose claims run no worker in a pod (`NO_POD`), one whose
   * pod is being replaced already (`POD_CHANGING`), an opening a limit of the project holds back
   * (`POD_UPDATE_LIMITED`, naming the limit), and a round no claim recorded within the daemon's
   * wait (`POD_UPDATE_NOT_RECORDED`, 503): a 200 is only ever a round a claim records. A request
   * that waits more than the daemon's bound (30 s) for a sweep's walk over the project's issues
   * opens nothing (`POD_UPDATE_BUSY`, 503), and its sentence names when to ask again; this client
   * reads no response header. While the project is paused it is refused (`PROJECT_PAUSED`, with
   * the pause that stands), and so is a pod a pause holds (`POD_HELD_BY_PAUSE`): the hold of a
   * pause that began after the daemon read the project as running, or a hold the resume left on a
   * pod none of whose roles is live yet. */
  readonly issuePodUpdate: (
    input: z.input<typeof LegionPodUpdateRequest>
  ) => Promise<z.output<typeof LegionPodUpdateResponse>>;
  /** The controller's `pause`: the daemon pauses its own project for `reason`, the effect
   * `legion project pause` runs for an operator. Every role but the controller stops, one in a
   * turn when that turn ends, and every start waits for the resume. The answer is the pause it
   * began. */
  readonly projectPause: (
    input: z.input<typeof LegionProjectPauseRequest>
  ) => Promise<z.output<typeof LegionProjectPauseResponse>>;
  /** The controller's `resume`: the daemon ends its project's pause, whoever made it, the effect
   * `legion project resume` runs for an operator. The answer is the pause it ended and how many
   * held rows it let out to run. */
  readonly projectResume: (
    input: z.input<typeof LegionProjectResumeRequest>
  ) => Promise<z.output<typeof LegionProjectResumeResponse>>;
  /** The controller's `set_p0_bound`: the daemon sets how many P0 issues may hold a slot at once
   * outside the cap, `bound`, or none with `unbounded`, from the change's own admission pass on, and
   * records who set it and why; the effect `legion project set-p0-bound` runs for an operator. The
   * answer is the bound in force, legion.yaml's and the live one. */
  readonly p0Bound: (
    input: z.input<typeof LegionProjectP0BoundRequest>
  ) => Promise<z.output<typeof LegionP0BoundResponse>>;
  /** The controller's `clear_p0_bound`: the daemon returns to legion.yaml's P0 bound, and records
   * who cleared the live one and why. The answer is the bound in force and legion.yaml's. */
  readonly p0BoundClear: (
    input: z.input<typeof LegionProjectP0BoundClearRequest>
  ) => Promise<z.output<typeof LegionP0BoundResponse>>;
  /** The controller's `replay_give_up`: the daemon applies again the messages its intake gave up
   * on that the selection names, `giveUp`, one record by its id, or `consumer` with `throughSeq`,
   * every open record of that consumer at or below that stream sequence, through the handling a
   * live delivery takes, the effect `legion intake replay` runs for an operator. The answer is
   * each record it took with how its replay ended, and how many open records of the selection
   * are left. */
  readonly intakeReplay: (
    input: z.input<typeof LegionIntakeReplayRequest>
  ) => Promise<z.output<typeof LegionIntakeReplayResponse>>;
  /** The controller's `discard_give_up`: the daemon closes the give-ups the selection names, for
   * `reason`, and applies nothing, the effect `legion intake discard` runs for an operator. The
   * answer is the id of each record it closed. */
  readonly intakeDiscard: (
    input: z.input<typeof LegionIntakeDiscardRequest>
  ) => Promise<z.output<typeof LegionIntakeDiscardResponse>>;
  readonly waveRelease: (
    input: z.input<typeof LegionWaveReleaseRequest>
  ) => Promise<z.output<typeof LegionWaveReleaseResponse>>;
  readonly phaseBackward: (
    input: z.input<typeof LegionPhaseBackwardRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  readonly phaseRetry: (
    input: z.input<typeof LegionPhaseRetryRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  readonly signOff: (
    input: z.input<typeof LegionSignOffRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  /** `report_defect` (the defect-calls entry): a defect of the change of an issue, found once its
   * task's last step passed, at the head it was seen at. The implementer reports in retro, the
   * merger in merging and in verifying until its completion lands, and the owning architect from
   * retro on. It moves no phase. The answer is the defect's id, whether the daemon recorded it,
   * found it the same as one it holds or joined it to one, and the generation of the change that
   * will carry it. */
  readonly reportDefect: (
    input: z.input<typeof LegionDefectReportRequest>
  ) => Promise<z.output<typeof LegionDefectReportResponse>>;
  /** The owning architect's `withdraw_defect` (the defect-calls entry): it takes back a defect by
   * its id, saying why. The answer is the defect's id and its state, `withdrawn`. */
  readonly withdrawDefect: (
    input: z.input<typeof LegionDefectWithdrawRequest>
  ) => Promise<z.output<typeof LegionDefectWithdrawResponse>>;
  readonly rootClose: (
    input: z.input<typeof LegionRootCloseRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  readonly childRerun: (
    input: z.input<typeof LegionChildRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  /** The owner's `register_plan` (contract 219): the plan in one version of a document of the
   * issue, which the daemon reads from Dispatch at that version. The answer is the plan it took;
   * a plan whose content it does not take is refused `PLAN_REFUSED`, every reason it names in the
   * sentence (the first reasons of each kind of cause and a count of the rest of that kind), and a plan
   * of an issue whose experiment's group takes no plan from its owner `PLAN_NOT_TAKEN`. */
  readonly planRegister: (
    input: z.input<typeof LegionPlanRegisterRequest>
  ) => Promise<z.output<typeof LegionPlanRegisterResponse>>;
  /** The owner's `stop_task` (contract 219): one task of the plan Legion runs for the issue stops,
   * and the issue is held `task-stopped` until a later version of the plan changes that task or,
   * on a root, a person sets the root `todo`. */
  readonly taskStop: (
    input: z.input<typeof LegionTaskStopRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
}

/** A request was answered with a status that is not success. `refusalShape` says by whom: true
 * where the body was the daemon's refusal shape (`LegionErrorResponse`), which is the daemon's
 * own answer, and false for any other body, such as a proxy's page in front of the daemon or an
 * empty body. In the refusal shape, `detail` is the sentence the daemon put under `error`, and
 * `code` the stable code workflow and credential refusals carry beside it; a refusal the daemon
 * wrote says what it refused. Any other body carries no code, and its `detail` names the body's
 * length in bytes, the contract this build speaks, the contract the daemon stated and which side
 * is behind, and quotes no part of the body, as `LegionDaemonContractError` does for a success
 * this build cannot read. `retryAfterMs` is the wait the answer's `Retry-After` header asked for
 * (`retryAfterMs` of `@legion/contracts/http-status`), 0 where it sent none: a proxy's 429 or 503
 * can send one, which the ready's retry waits within its bound (`callReadyWithRetry`). */
export class LegionDaemonApiError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly code: string | undefined,
    readonly detail: string,
    readonly refusalShape: boolean,
    readonly retryAfterMs: number
  ) {
    super(
      `${method} ${path} failed with ${status}${code === undefined ? "" : ` ${code}`}: ${detail}`
    );
    this.name = "LegionDaemonApiError";
  }
}

/** Which of the two builds is behind, for the refusals of an answer this build cannot read (the
 * success `LegionDaemonContractError` names, and the non-success `LegionDaemonApiError` that is
 * not the daemon's refusal shape): this build's contract and the one the daemon stated. A process
 * whose contract differs from the daemon's, above it or below it, is replaced. An Oh My Pi
 * role's is relaunched onto the daemon's own plugin by the daemon's stale-plugin lane
 * (`refreshPlugin` in the daemon's `internal/supervise/plugin.go`). A Claude Code role in a pod
 * installs no plugin (`NoPluginInstall`): its plugin is its image's own claude-envoy, so the
 * update round over its pod replaces it. So a process below the daemon waits for its
 * replacement. A process above it is told that the daemon is older and nothing of a cure: the
 * lane relaunches an Oh My Pi process above the daemon onto the daemon's plugin too, which is
 * older than this build. */
function contractStanding(mine: number, daemon: number | undefined): string {
  if (daemon === undefined) {
    return `This build speaks daemon API contract ${mine}. The daemon stated no contract, so this build cannot say which side is behind.`;
  }
  if (mine < daemon) {
    return `This build speaks daemon API contract ${mine}. The daemon speaks ${daemon}. This process is behind the daemon and waits for its replacement.`;
  }
  if (mine > daemon) {
    return `This build speaks daemon API contract ${mine}. The daemon speaks ${daemon}. The daemon is older than this build.`;
  }
  return `This build speaks daemon API contract ${mine}. The daemon speaks ${daemon}. Neither side is behind.`;
}

/** A non-success body that is not the daemon's refusal shape, as the refusal names it: by its
 * length in bytes of UTF-8, quoting no part of it. A proxy's page, or a body carrying a value,
 * must not reach the model. */
function unreadableBody(text: string): string {
  return `a body of ${new TextEncoder().encode(text).length} bytes`;
}

/** A success the daemon API contract does not describe: the path of each field that is wrong,
 * the contract this build speaks, the contract the daemon stated, and which side is behind. The
 * body is never quoted: a registration's carries the claim's secret. */
export class LegionDaemonContractError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly pluginContract: number,
    readonly daemonContract: number | undefined,
    readonly problem: string
  ) {
    super(
      `${method} ${path} answered ${status} with a body the daemon API contract does not describe: ${problem}. ${contractStanding(pluginContract, daemonContract)}`
    );
    this.name = "LegionDaemonContractError";
  }
}

// Bounds retries of the transient `claims/ready` request: three attempts, the waits between them
// 1 s, or as long as the answer's `Retry-After` asks but never longer than 30 s. So the retry
// waits at most 60 s, well inside the daemon's registration deadline, whose default is 360 s
// (`worker_boot_timeout_seconds` 120 times `worker_boot_registration_deadline_intervals` 3), past
// which the daemon relaunches a pane that never said it was ready.
const READY_RETRY_ATTEMPTS = 3;
const READY_RETRY_DELAY_MS = 1_000;
const READY_RETRY_AFTER_MAX_MS = 30_000;

function isNetworkOrTimeoutError(error: unknown): boolean {
  if (!(error instanceof Error)) return false;
  if (error.name === "NetworkError" || error.name === "TimeoutError") return true;

  const code = "code" in error && typeof error.code === "string" ? error.code : undefined;
  return (
    code === "ConnectionRefused" ||
    code === "ConnectionTimeout" ||
    code === "EAI_AGAIN" ||
    code === "ECONNREFUSED" ||
    code === "ECONNRESET" ||
    code === "EHOSTUNREACH" ||
    code === "ENETUNREACH" ||
    code === "ENOTFOUND" ||
    code === "ETIMEDOUT" ||
    (error instanceof TypeError &&
      (error.message === "Failed to fetch" || error.message === "fetch failed"))
  );
}

/** Runs a ready request, retrying only one that can pass on its own: an outage (`outage`: a 5xx, a
 * 408 or a 429) or a network failure. It asks an outage again whatever its body, a body in the
 * daemon's refusal shape included, where `daemonUnreachable` asks none such again: the ready
 * route's own 503 is the daemon's refusal and means "try the ready again" (the daemon could not
 * queue the publication of the issue's role sessions: `ready` in
 * packages/daemon/internal/api/claims.go). The daemon answers no 408 and no 429 itself, so either
 * is a proxy between the session and the daemon. Each Legion host's claim (pi-legion's claim
 * session, claude-envoy's Legion claim) sends its `claims/ready` through it, at boot and after each
 * role regain. It makes `READY_RETRY_ATTEMPTS` attempts, and waits between them 1 s, or what the
 * answer's `Retry-After` asks for where that is longer, but never longer than
 * `READY_RETRY_AFTER_MAX_MS`: a proxy that asks for longer is asked again at that bound. Each
 * failed attempt is said on `log`, the host's own writer.
 *
 * `signal` aborts when the host stops (R24 of dispatch://INFRA-3589). From then on nothing is
 * said and no attempt is made: a wait for the next attempt ends at once, and the call throws the
 * last attempt's error. `sleep` is the wait, a test's own, which ends at once when the signal it
 * is given aborts. */
export async function callReadyWithRetry(
  label: string,
  call: () => Promise<void>,
  host: { readonly log: (line: string) => void; readonly signal?: AbortSignal | undefined },
  sleep: (ms: number, signal: AbortSignal | undefined) => Promise<void> = (ms, signal) => {
    const waited = Promise.withResolvers<void>();
    const timer = setTimeout(waited.resolve, ms);
    const stop = (): void => {
      clearTimeout(timer);
      waited.resolve();
    };
    if (signal?.aborted === true) stop();
    else signal?.addEventListener("abort", stop, { once: true });
    return waited.promise.finally(() => signal?.removeEventListener("abort", stop));
  }
): Promise<void> {
  const { log, signal } = host;
  // Read afresh at each use: the abort comes while an attempt or a wait is pending.
  const stopped = (): boolean => signal?.aborted === true;
  for (let attempt = 1; ; attempt++) {
    try {
      await call();
      return;
    } catch (error) {
      const retryable =
        error instanceof LegionDaemonApiError
          ? outage(error.status)
          : isNetworkOrTimeoutError(error);
      if (!retryable || stopped()) throw error;
      if (attempt === READY_RETRY_ATTEMPTS) {
        log(`${label} failed after ${attempt} attempts: ${messageFor(error)}`);
        throw error;
      }
      const asked = error instanceof LegionDaemonApiError ? error.retryAfterMs : 0;
      const wait = Math.min(Math.max(READY_RETRY_DELAY_MS, asked), READY_RETRY_AFTER_MAX_MS);
      log(
        `${label} failed (attempt ${attempt}/${READY_RETRY_ATTEMPTS}), retrying in ${wait / 1_000} s: ${messageFor(error)}`
      );
      await sleep(wait, signal);
      if (stopped()) throw error;
    }
  }
}

/** What a call retried across a daemon restart is told: once, on its first connection failure,
 * the sentence for the host to show (Oh My Pi's `onUpdate` reaches its TUI and RPC client, not
 * the model); the abort signal that ends the wait, checked before every attempt; and the clock it
 * waits on (a test's own). */
export interface DaemonRestartRetry {
  readonly onRetrying?: (message: string) => void;
  readonly signal?: AbortSignal;
  readonly now?: () => number;
  readonly sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
}

/** How long a call waits for a restarting daemon, and its backoff from first to longest wait. */
export const DAEMON_RESTART_BUDGET_MS = 10 * 60_000;
const DAEMON_RESTART_FIRST_DELAY_MS = 500;
const DAEMON_RESTART_MAX_DELAY_MS = 30_000;

/** Why a request got no answer from the daemon: `refused` never reached it (nothing was sent: a
 * refused connection, or a 429 whose body is not the daemon's refusal shape, a proxy in front of
 * the daemon turning the request away before it forwarded anything); `dropped` may have reached it
 * (a connection reset or closed before a response, or a 5xx or a 408 whose body is not the
 * daemon's refusal shape: a proxy's page, which it may have sent once the daemon had the request,
 * as a proxy whose own timeout ran out after it forwarded the request answers 408). Each of those
 * statuses is an outage (`outage`), and `mayHaveReachedServer` says which of the two it is.
 * Undefined for everything else, which is never retried: a body in the daemon's refusal shape
 * (`refusalShape`), which is the daemon's answer whatever its status and whether or not it
 * carries a code, such as a claim route's 500 that names no code or the 502 `DISPATCH_FAILED`;
 * and any other body whose status is not an outage, which answers the same request the same way. */
export function daemonUnreachable(error: unknown): "refused" | "dropped" | undefined {
  if (error instanceof LegionDaemonApiError) {
    if (error.refusalShape || !outage(error.status)) return undefined;
    return mayHaveReachedServer(error.status) ? "dropped" : "refused";
  }
  if (!(error instanceof Error)) return undefined;
  const code = "code" in error && typeof error.code === "string" ? error.code : undefined;
  if (code === "ConnectionRefused" || code === "ECONNREFUSED") return "refused";
  if (code === "ConnectionClosed" || code === "ECONNRESET" || code === "EPIPE") return "dropped";
  return error instanceof TypeError &&
    (error.message === "Failed to fetch" || error.message === "fetch failed")
    ? "dropped"
    : undefined;
}

/**
 * Runs `call` until it gets the daemon's answer, retrying with capped exponential backoff while
 * the daemon is unreachable (`daemonUnreachable`), for up to `DAEMON_RESTART_BUDGET_MS` or until
 * `signal` aborts; an abort during a wait sends no further attempt. Only a call that the daemon
 * applies at most once however often it arrives may run through it: `handoff_complete` (a repeat
 * is answered HANDOFF_ALREADY_RECORDED, or, once the completion moved the issue, refused as no
 * longer the role's phase), `sign_off` (a repeat finds the issue out of accepting and changes
 * nothing), `register_gate` (its fact is keyed by the gate's issue, generation, document,
 * version and Dispatch sequence, and by its basis's document and version where it names one; a
 * repeat after the sequence moved is a new fact, which the gate takes as a re-registration of the
 * same document and tells no one again), `register_plan` (the daemon judges every give against its
 * store: a repeat of a give it applied is answered as taken while that version is in force, and
 * writes nothing), `stop_task` (a repeat finds the task stopped and changes nothing) and
 * `claims/turn-outcome` (one outcome per claim, generation and sequence, so a repeat writes the
 * cell its first attempt wrote). `call` is the whole
 * operation, grant mint included, since grants live in the daemon's memory and do not survive
 * its restart. `onRetrying` is told once. A daemon refusal after a dropped attempt is told that
 * the dropped one may have been applied.
 */
export async function retryWhileDaemonRestarts<T>(
  label: string,
  call: () => Promise<T>,
  options: DaemonRestartRetry = {}
): Promise<T> {
  const { signal } = options;
  const now = options.now ?? Date.now;
  const sleep =
    options.sleep ??
    ((ms: number, abort?: AbortSignal) => {
      const wait = Promise.withResolvers<void>();
      const timer = setTimeout(wait.resolve, ms);
      abort?.addEventListener(
        "abort",
        () => {
          clearTimeout(timer);
          wait.resolve();
        },
        { once: true }
      );
      return wait.promise;
    });
  const deadline = now() + DAEMON_RESTART_BUDGET_MS;
  let delay = DAEMON_RESTART_FIRST_DELAY_MS;
  let told = false;
  let dropped = false;
  let lastFailure: unknown;
  for (;;) {
    // An abort during the wait sends nothing more: a write the agent cancelled is never applied.
    if (signal?.aborted && lastFailure !== undefined) {
      throw new Error(
        `${label}: the Legion daemon was unreachable (${messageFor(lastFailure)}) and the call was aborted${dropped ? "; an attempt lost its connection before the daemon answered and may have been applied, so read the record before calling again" : "; call it again once the daemon is back"}`
      );
    }
    try {
      return await call();
    } catch (error) {
      const unreachable = daemonUnreachable(error);
      if (unreachable === undefined) {
        if (
          dropped &&
          (error instanceof LegionDaemonApiError || error instanceof LegionDaemonContractError)
        ) {
          error.message += ` (an earlier attempt of ${label} lost its connection before the daemon answered, and may have been applied: read_record of your own issue says whether it landed)`;
        }
        throw error;
      }
      lastFailure = error;
      dropped ||= unreachable === "dropped";
      const remaining = deadline - now();
      if (remaining <= 0) {
        throw new Error(
          `${label}: the Legion daemon was unreachable (${messageFor(error)}) and stayed so for ${DAEMON_RESTART_BUDGET_MS / 60_000} minutes${dropped ? "; an attempt lost its connection before the daemon answered and may have been applied, so read the record before calling again" : "; call it again once the daemon is back"}`
        );
      }
      if (!told && !signal?.aborted) {
        told = true;
        const message = `The Legion daemon is unreachable, most likely restarting (${messageFor(error)}); ${label} is being retried for up to ${DAEMON_RESTART_BUDGET_MS / 60_000} minutes.`;
        console.error(`[legion] ${message}`);
        options.onRetrying?.(message);
      }
      if (!signal?.aborted) await sleep(Math.min(delay, remaining), signal);
      delay = Math.min(delay * 2, DAEMON_RESTART_MAX_DELAY_MS);
    }
  }
}

/** `body` read through `schema`, or the reason it is not what the schema describes. The answer
 * schemas ignore a key they do not name, so a body carrying a field this build's contract has no
 * name for is read and that field left out of the value; what is refused is a body that is not
 * JSON, a missing required field, and a field of a type the schema does not describe. A body that
 * is not JSON is said to be so and no more: the parser's own message quotes the body (Bun's names
 * up to about 200 characters of a bare word), and `LegionDaemonContractError` quotes none of it,
 * while a non-success answer's refusal shows only the body's length (`unreadableBody`). */
function parseBody<T>(
  schema: z.ZodType<T>,
  body: string
): { readonly value: T } | { readonly problem: string } {
  let json: unknown;
  try {
    json = JSON.parse(body);
  } catch {
    // The parser's message quotes the word it stopped at, which whatever answered chose.
    return { problem: "not JSON" };
  }
  const parsed = schema.safeParse(json);
  if (parsed.success) return { value: parsed.data };
  return {
    problem: parsed.error.issues
      .map((issue) => `${issue.path.join(".") || "(root)"}: ${issue.message}`)
      .join("; "),
  };
}

/** What `createLegionDaemonClient` needs of its host: the contract this build speaks, and the
 * `fetch` the requests go through. `pluginContract` is required, and each host passes its own
 * manifest's `legion.daemonApiVersion` (`packages/pi-legion/extensions/legion.ts`,
 * `packages/claude-envoy/src/envoy-channel-server.ts`): a client that stated nothing would be
 * served the shape of whatever pod the daemon places it in, which is the defect
 * dispatch://INFRA-2694 reports. It is the one name for this number: the registration sends it
 * as `pluginContract`, `PluginBuild` holds it under that name, and `read_build` answers it
 * beside the daemon's own. */
export interface LegionDaemonClientOptions {
  readonly pluginContract: number;
  readonly fetch?: typeof fetch;
}

/** The request header a caller states its own contract in
 * (`packages/daemon/internal/api/shaping.go`), and the response header the daemon states its own
 * in (`packages/daemon/internal/api/server.go`). */
const PLUGIN_CONTRACT_HEADER = "Legion-Plugin-Contract";
const DAEMON_CONTRACT_HEADER = "Legion-Daemon-Contract";

/** The contract the daemon stated on an answer, or `undefined` where it stated none or stated
 * what is no positive whole number. A daemon older than the header states none. */
function statedContract(headers: Headers): number | undefined {
  const stated = headers.get(DAEMON_CONTRACT_HEADER);
  if (stated === null) return undefined;
  const contract = Number(stated);
  return Number.isSafeInteger(contract) && contract > 0 ? contract : undefined;
}

export function createLegionDaemonClient(
  baseUrl: string,
  options: LegionDaemonClientOptions
): LegionDaemonClient {
  const endpoint = withoutTrailingSlashes(baseUrl);
  const pluginContract = options.pluginContract;
  const fetchFn = options.fetch ?? fetch;

  /** The response's status, body and stated contract once the daemon answered success; a
   * refusal throws. Every request carries this build's contract, a GET as much as a POST: the
   * daemon serves each caller the shape that contract describes, and a caller that states none
   * is served the shape of the pod the daemon places it in. */
  const call = async (
    method: "GET" | "POST",
    path: string,
    body?: object,
    signal?: AbortSignal
  ): Promise<{
    readonly status: number;
    readonly text: string;
    readonly daemonContract: number | undefined;
  }> => {
    const response = await fetchFn(`${endpoint}${path}`, {
      method,
      headers: {
        [PLUGIN_CONTRACT_HEADER]: String(pluginContract),
        ...(body === undefined ? {} : { "Content-Type": "application/json" }),
      },
      ...(body === undefined ? {} : { body: JSON.stringify(body) }),
      ...(signal === undefined ? {} : { signal }),
    });
    const text = await response.text();
    const daemonContract = statedContract(response.headers);
    if (response.ok) return { status: response.status, text, daemonContract };
    const asked = retryAfterMs(response.headers);
    const refusal = parseBody(LegionErrorResponse, text);
    if ("value" in refusal) {
      throw new LegionDaemonApiError(
        method,
        path,
        response.status,
        refusal.value.code,
        refusal.value.error,
        true,
        asked
      );
    }
    // A body that is not the daemon's refusal was written by something in its place, which can
    // echo the request back: it is named by its length alone (`unreadableBody`), never quoted.
    throw new LegionDaemonApiError(
      method,
      path,
      response.status,
      undefined,
      `${unreadableBody(text)} is not the daemon's refusal shape (${refusal.problem}). ${contractStanding(pluginContract, daemonContract)}`,
      false,
      asked
    );
  };

  const read = async <T>(
    method: "GET" | "POST",
    path: string,
    schema: z.ZodType<T>,
    body?: object,
    signal?: AbortSignal
  ): Promise<T> => {
    const { status, text, daemonContract } = await call(method, path, body, signal);
    const parsed = parseBody(schema, text);
    if ("problem" in parsed) {
      throw new LegionDaemonContractError(
        method,
        path,
        status,
        pluginContract,
        daemonContract,
        parsed.problem
      );
    }
    return parsed.value;
  };

  /** Parses the request before it leaves the pane, then reads the route's success body. */
  const post = <Input, Output>(
    path: string,
    request: z.ZodType<Input>,
    response: z.ZodType<Output>,
    body: Input
  ): Promise<Output> => read("POST", path, response, request.parse(body) as object);

  /** The claim routes that answer 204 and nothing else. */
  const acknowledge = async (path: string, body: object, signal?: AbortSignal): Promise<void> => {
    const { status, text, daemonContract } = await call("POST", path, body, signal);
    if (status !== 204 || text !== "") {
      throw new LegionDaemonContractError(
        "POST",
        path,
        status,
        pluginContract,
        daemonContract,
        "the route answers 204 with no body"
      );
    }
  };

  return {
    state: (reading) =>
      read("GET", "/legion/v1/state", LegionStateResponse, undefined, reading?.signal),
    record: async (issue, reading) => {
      try {
        return await read(
          "GET",
          `/legion/v1/issues/${encodeURIComponent(issue)}/record`,
          LegionIssueRecordResponse,
          undefined,
          reading?.signal
        );
      } catch (error) {
        // The one refusal that is an answer: the daemon records nothing for this issue. Every
        // other refusal is a read that failed and is thrown on, so a caller's `undefined` is
        // the absent record and nothing else.
        if (
          error instanceof LegionDaemonApiError &&
          error.status === 404 &&
          error.code === "RECORD_NOT_FOUND"
        ) {
          return undefined;
        }
        throw error;
      }
    },
    health: (reading) => read("GET", "/healthz", LegionHealthResponse, undefined, reading?.signal),
    // The header alone, parsed from an answer whose body this build may be unable to read: a
    // reader that reports the two numbers side by side must reach the daemon's number whatever
    // its health body carries, so this parses no schema.
    daemonContract: async (reading) =>
      (await call("GET", "/healthz", undefined, reading?.signal)).daemonContract,
    register: (input) => read("POST", "/legion/v1/claims/register", LegionRegisterResponse, input),
    registerController: (input) =>
      read("POST", "/legion/v1/claims/register", LegionControllerRegisterResponse, input),
    ready: (input, options) => acknowledge("/legion/v1/claims/ready", input, options?.signal),
    readyForUpdate: (input) => acknowledge("/legion/v1/claims/ready-for-update", input),
    exit: (input) => acknowledge("/legion/v1/claims/exit", input),
    turnOutcome: (input) =>
      post(
        "/legion/v1/claims/turn-outcome",
        LegionTurnOutcomeRequest,
        LegionTurnOutcomeResponse,
        input
      ),
    activity: (input) => acknowledge("/legion/v1/claims/activity", input),
    releaseRecords: (input) =>
      post(
        "/legion/v1/claims/release-records",
        LegionReleaseRecordsRequest,
        LegionReleaseRecordsResponse,
        input
      ),
    grant: (input) => post("/legion/v1/grants", LegionGrantRequest, LegionGrantResponse, input),
    controllerGrant: (input) =>
      post("/legion/v1/grants", LegionControllerGrantRequest, LegionGrantResponse, input),
    handoffComplete: (input) =>
      post(
        "/legion/v1/handoff/complete",
        LegionHandoffCompleteRequest,
        LegionHandoffCompleteResponse,
        input
      ),
    issueStatus: (input) =>
      post("/legion/v1/issues/status", LegionIssueStatusRequest, LegionEmptyResponse, input),
    freshSession: (input) =>
      post(
        "/legion/v1/claims/fresh-session",
        LegionClaimRequest,
        LegionFreshSessionResponse,
        input
      ),
    compact: (input) =>
      post("/legion/v1/claims/compact", LegionClaimRequest, LegionCompactResponse, input),
    stopAgent: (input) =>
      post("/legion/v1/agents/stop", LegionClaimRequest, LegionAgentActionResponse, input),
    retryAgent: (input) =>
      post("/legion/v1/agents/retry", LegionClaimRequest, LegionAgentActionResponse, input),
    suspendAgent: (input) =>
      post("/legion/v1/agents/suspend", LegionClaimRequest, LegionAgentActionResponse, input),
    resumeAgent: (input) =>
      post("/legion/v1/agents/resume", LegionClaimRequest, LegionAgentActionResponse, input),
    issuePodUpdate: (input) =>
      post("/legion/v1/issues/pod-update", LegionPodUpdateRequest, LegionPodUpdateResponse, input),
    projectPause: (input) =>
      post(
        "/legion/v1/project/pause",
        LegionProjectPauseRequest,
        LegionProjectPauseResponse,
        input
      ),
    projectResume: (input) =>
      post(
        "/legion/v1/project/resume",
        LegionProjectResumeRequest,
        LegionProjectResumeResponse,
        input
      ),
    p0Bound: (input) =>
      post(
        "/legion/v1/project/p0-bound",
        LegionProjectP0BoundRequest,
        LegionP0BoundResponse,
        input
      ),
    p0BoundClear: (input) =>
      post(
        "/legion/v1/project/p0-bound/clear",
        LegionProjectP0BoundClearRequest,
        LegionP0BoundResponse,
        input
      ),
    intakeReplay: (input) =>
      post(
        "/legion/v1/intake/replay",
        LegionIntakeReplayRequest,
        LegionIntakeReplayResponse,
        input
      ),
    intakeDiscard: (input) =>
      post(
        "/legion/v1/intake/discard",
        LegionIntakeDiscardRequest,
        LegionIntakeDiscardResponse,
        input
      ),
    gateRegister: (input) =>
      post("/legion/v1/gates/register", LegionGateRegisterRequest, LegionEmptyResponse, input),
    waveRelease: (input) =>
      post("/legion/v1/waves/release", LegionWaveReleaseRequest, LegionWaveReleaseResponse, input),
    phaseBackward: (input) =>
      post("/legion/v1/phase/backward", LegionPhaseBackwardRequest, LegionEmptyResponse, input),
    phaseRetry: (input) =>
      post("/legion/v1/phase/retry", LegionPhaseRetryRequest, LegionEmptyResponse, input),
    signOff: (input) =>
      post("/legion/v1/signoff", LegionSignOffRequest, LegionEmptyResponse, input),
    reportDefect: (input) =>
      post(
        "/legion/v1/defects/report",
        LegionDefectReportRequest,
        LegionDefectReportResponse,
        input
      ),
    withdrawDefect: (input) =>
      post(
        "/legion/v1/defects/withdraw",
        LegionDefectWithdrawRequest,
        LegionDefectWithdrawResponse,
        input
      ),
    rootClose: (input) =>
      post("/legion/v1/roots/close", LegionRootCloseRequest, LegionEmptyResponse, input),
    childRerun: (input) =>
      post("/legion/v1/children/rerun", LegionChildRequest, LegionEmptyResponse, input),
    planRegister: (input) =>
      post(
        "/legion/v1/plans/register",
        LegionPlanRegisterRequest,
        LegionPlanRegisterResponse,
        input
      ),
    taskStop: (input) =>
      post("/legion/v1/tasks/stop", LegionTaskStopRequest, LegionEmptyResponse, input),
  };
}
