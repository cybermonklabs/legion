import {
  type LegionActivityRequest,
  LegionChildRequest,
  LegionClaimRequest,
  LegionCompactResponse,
  LegionControllerGrantRequest,
  LegionControllerRegisterResponse,
  type LegionControllerRegistration,
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
  type LegionIssue,
  LegionIssueRecordResponse,
  LegionIssueStatusRequest,
  LegionPhaseBackwardRequest,
  LegionPhaseRetryRequest,
  LegionProjectPauseRequest,
  LegionProjectPauseResponse,
  LegionProjectResumeRequest,
  LegionProjectResumeResponse,
  type LegionReadyForUpdateRequest,
  type LegionReadyRequest,
  LegionRegisterResponse,
  type LegionRegistration,
  LegionRelaunchRequest,
  LegionRelaunchResponse,
  LegionReleaseRecordsRequest,
  LegionReleaseRecordsResponse,
  LegionRootCloseRequest,
  LegionSignOffRequest,
  type LegionState,
  LegionStateResponse,
  LegionTurnOutcomeRequest,
  LegionTurnOutcomeResponse,
  LegionWaveReleaseRequest,
  LegionWaveReleaseResponse,
} from "@legion/contracts/legion-api";
import { withoutTrailingSlashes } from "@legion/contracts/trailing-slash";
import { messageFor } from "@legion/envoy-client/errors";
import type { z } from "zod";
import { daemonWaitBegan } from "./daemon-wait";

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
 * `packages/daemon/internal/claim/wire.go`, which refuses a member it does not read. There is no
 * secret recovery: the daemon persists a registration's capability before it answers, so a
 * restart forgets no secret.
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

/** What a request may be handed beside its body. `signal` aborts it, its answer's body
 * included, and a request whose signal is aborted before it is sent, or by the time its answer
 * is read, throws the signal's reason rather than sending or answering: the step after it
 * belongs to whoever stopped waiting for it, and must not run (`retryWhileDaemonRestarts`). */
export interface DaemonRequestOptions {
  readonly signal?: AbortSignal;
}

export interface LegionDaemonClient {
  /** The daemon's whole state. `signal` aborts the request, its answer's body included: a caller
   * that stops waiting for a daemon that does not answer (the phase reminder's bounded record
   * check, an attempt `retryWhileDaemonRestarts` gave up on) aborts it rather than leave it open. */
  readonly state: (options?: DaemonRequestOptions) => Promise<LegionState>;
  /** One issue's record, the value the whole state holds under `issues.<key>`, or `undefined`
   * for an issue the daemon records nothing of, which it answers 404 `RECORD_NOT_FOUND`. Every
   * other refusal throws, so a caller's `undefined` is the absent record and its `catch` is the
   * read that failed. `signal` aborts the request as `state`'s does. */
  readonly record: (
    issue: string,
    options?: DaemonRequestOptions
  ) => Promise<LegionIssue | undefined>;
  /** That the daemon is serving, and the project it serves: one word for the controller's boot
   * check, which read the whole state for it before. */
  readonly health: (options?: DaemonRequestOptions) => Promise<LegionHealth>;
  /** The contract the daemon states on its answer, or `undefined` where it states none. It
   * reads the health route's header and parses no body, so it answers for a daemon whose health
   * body this build cannot read: the two numbers are what a reader compares. */
  readonly daemonContract: (options?: DaemonRequestOptions) => Promise<number | undefined>;
  /** The agent's first call. `signal` aborts the request: a boot that waits out a daemon that
   * is not serving ends an attempt the daemon took and never answered before it sends the next
   * one, so the daemon never holds two registrations of this session in flight and the secret
   * this client reads is the one the daemon applied last (`retryWhileDaemonRestarts`). */
  readonly register: (
    input: RegisterInput,
    options?: DaemonRequestOptions
  ) => Promise<LegionRegistration>;
  /** The same route, for the operator-launched controller: `bootToken` is the capability `legion
   * controller start` fetched, and the answer is the controller's registration. */
  readonly registerController: (input: RegisterInput) => Promise<LegionControllerRegistration>;
  /** The agent saying it can be prompted. `signal` aborts the request, its answer's body
   * included: a caller that stops waiting for a daemon that does not answer (the capability
   * schedule's bounded send) aborts it rather than leave it open. */
  readonly ready: (input: ReadyInput, options?: DaemonRequestOptions) => Promise<void>;
  readonly readyForUpdate: (input: ReadyForUpdateInput) => Promise<void>;
  readonly exit: (input: ExitInput) => Promise<void>;
  /** `claim.TurnOutcomeRequest`: how one of the claim's turns ended, under the sequence the
   * reporting plugin counted itself (`packages/pi-legion/src/turn-outcome.ts`). The answer is the
   * trailing run of refused turns the daemon derived and whether this report failed the claim.
   * `signal` is the attempt's, as `grant`'s is. */
  readonly turnOutcome: (
    input: z.input<typeof LegionTurnOutcomeRequest>,
    options?: DaemonRequestOptions
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
  /** A grant for one write. `signal` is the attempt's, for a write that waits out a daemon
   * restart (`retryWhileDaemonRestarts`): the grant, and the post it is minted for, are that
   * attempt's own, and an attempt given up on mints and posts nothing more. */
  readonly grant: (
    input: z.input<typeof LegionGrantRequest>,
    options?: DaemonRequestOptions
  ) => Promise<z.output<typeof LegionGrantResponse>>;
  readonly controllerGrant: (
    input: z.input<typeof LegionControllerGrantRequest>
  ) => Promise<z.output<typeof LegionGrantResponse>>;
  readonly handoffComplete: (
    input: z.input<typeof LegionHandoffCompleteRequest>,
    options?: DaemonRequestOptions
  ) => Promise<z.output<typeof LegionHandoffCompleteResponse>>;
  readonly issueStatus: (
    input: z.input<typeof LegionIssueStatusRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  /** The controller's `fresh_session`: the claim on `role` of `issue` leaves its recorded
   * session behind and starts again in a new one, the effect
   * `legion claims retry --fresh-session` runs for an operator. The answer is empty wherever
   * the claim came back and its start ran. `phaseNotRestarted` carries the workflow's refusal
   * of the phase restart, where the claim came back owing no task, or the machine's refusal of
   * a step that would have started the claim again, where this request stopped it. That
   * sentence names the state the claim stands in: read it in `read_state`, and ask for a fresh
   * session again only where the claim is suspended. */
  readonly freshSession: (
    input: z.input<typeof LegionClaimRequest>
  ) => Promise<z.output<typeof LegionFreshSessionResponse>>;
  readonly gateRegister: (
    input: z.input<typeof LegionGateRegisterRequest>,
    options?: DaemonRequestOptions
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  /** The controller's `compact`: the claim on `role` of `issue` has its Oh My Pi session
   * compacted and is then sent the task it owes again, the effect `legion claims compact` runs
   * for an operator. The answer says whether the session was compacted and, when it was not,
   * why; the claim came back and whatever task it owes went again either way. */
  readonly compact: (
    input: z.input<typeof LegionClaimRequest>
  ) => Promise<z.output<typeof LegionCompactResponse>>;
  /** The controller's `relaunch`: the claim `claim` names, which Envoy cannot reach, is started
   * again on its same session, as the daemon relaunches a claim after a death, uncharged. The
   * answer is empty; every refusal is a `LegionDaemonApiError` with the daemon's code. No retry
   * wraps it: a relaunch that may have run is not asked for again, and the daemon refuses a
   * second one of the same claim within 6 hours. */
  readonly relaunch: (
    input: z.input<typeof LegionRelaunchRequest>
  ) => Promise<z.output<typeof LegionRelaunchResponse>>;
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
    input: z.input<typeof LegionSignOffRequest>,
    options?: DaemonRequestOptions
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  readonly rootClose: (
    input: z.input<typeof LegionRootCloseRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  readonly childPark: (
    input: z.input<typeof LegionChildRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
  readonly childRerun: (
    input: z.input<typeof LegionChildRequest>
  ) => Promise<z.output<typeof LegionEmptyResponse>>;
}

/** The daemon answered with a status that is not success. `detail` is the sentence it put
 * under `error`; workflow refusals also carry their stable `code`. A refusal the daemon wrote
 * says what it refused. A non-success answer this build cannot read as a refusal carries no code,
 * and its `detail` names the body's length in bytes, the contract this build speaks, the contract
 * the daemon stated and which side is behind, and quotes no part of the body, as
 * `LegionDaemonContractError` does for a success this build cannot read. */
export class LegionDaemonApiError extends Error {
  constructor(
    readonly method: string,
    readonly path: string,
    readonly status: number,
    readonly code: string | undefined,
    readonly detail: string
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

// Bounds retries of a `claims/ready` the daemon itself refused with a 5xx.
const READY_RETRY_ATTEMPTS = 3;
const READY_RETRY_DELAY_MS = 1_000;

/** Runs a ready request, retrying one the daemon itself refused with a 5xx that can pass on its
 * own. An answer that says the daemon is not serving at all (`daemonUnreachable`) is handed
 * back rather than spent in three seconds: the wait around this call owns that outage
 * (`retryWhileDaemonRestarts`). Each Legion host's claim (pi-legion's claim session,
 * claude-envoy's Legion claim) sends its `claims/ready` through it, at boot and after each role
 * regain. */
export async function callReadyWithRetry(
  label: string,
  call: () => Promise<void>,
  options: DaemonRestartRetry = {}
): Promise<void> {
  const sleep = options.sleep ?? waitFor;
  for (let attempt = 1; ; attempt++) {
    try {
      await call();
      return;
    } catch (error) {
      const retryable =
        error instanceof LegionDaemonApiError &&
        error.status >= 500 &&
        error.status < 600 &&
        daemonUnreachable(error) === undefined;
      if (!retryable) throw error;
      if (attempt === READY_RETRY_ATTEMPTS) {
        console.error(`[legion] ${label} failed after ${attempt} attempts: ${messageFor(error)}`);
        throw error;
      }
      console.error(
        `[legion] ${label} failed (attempt ${attempt}/${READY_RETRY_ATTEMPTS}), retrying: ${messageFor(error)}`
      );
      await sleep(READY_RETRY_DELAY_MS);
    }
  }
}

/** What a call that waits for a daemon that is not serving is told: once, on its first such
 * answer, the sentence for the host to show (Oh My Pi's `onUpdate` reaches its TUI and RPC
 * client, not the model); the abort signal that ends the call, which ends the attempt waiting on
 * the daemon as well as the wait between attempts; the clock it waits on (a test's own); how one
 * attempt's own limit is timed, which a test drives rather than waiting out; and the tool call
 * that waits, if a tool call does. */
export interface DaemonRestartRetry {
  readonly onRetrying?: (message: string) => void;
  readonly signal?: AbortSignal;
  readonly now?: () => number;
  readonly sleep?: (ms: number, signal?: AbortSignal) => Promise<void>;
  /** Answers once `ms` have passed and the attempt is overdue; `answered` is aborted the moment
   * the attempt settles or steps outside the daemon (`OutsideTheDaemon`), which ends the timer
   * without that answer. */
  readonly attemptLimit?: (ms: number, answered: AbortSignal) => Promise<void>;
  /** The tool call this wait runs inside (the `legion` tool's `handoff_complete`, `sign_off`,
   * `register_gate`), under which the call's time on the daemon is recorded so the worker's own
   * stall watch takes it off that call's clock and no other's (`src/daemon-wait.ts`). A wait no
   * tool call owns — the boot's, a role regain's `claims/ready`, a turn's outcome — names none
   * and records nothing, since no call is stopped by it. */
  readonly toolCallId?: string;
}

/** How long a call waits for a daemon that is not serving, and its backoff from first to longest
 * wait. An hour covers the longest outage on record, the 50 minutes of 2026-11-12. The daemon holds
 * a copy of it (`packages/daemon/internal/config/stalldefaults.go`, StallDaemonWait), because its
 * backstop bound must stay above the whole ladder a working plugin runs and a call that waits here
 * pauses the worker's own stall watch for as long (`src/daemon-wait.ts`); the two are pinned to
 * each other by `packages/contracts/fixtures/daemon-api/stall-defaults.json`. */
export const DAEMON_RESTART_BUDGET_MS = 60 * 60_000;
const DAEMON_RESTART_FIRST_DELAY_MS = 500;
const DAEMON_RESTART_MAX_DELAY_MS = 30_000;

/** How long one attempt may go unanswered before it is ended and made again. A daemon that
 * accepts a connection and never answers it is a daemon that is not serving, and nothing else
 * in the stack says so: a request with no limit of its own would hold the call until the
 * worker's stall watch ended the turn. */
export const DAEMON_ATTEMPT_LIMIT_MS = 5 * 60_000;

/** An attempt the daemon took and did not answer within `DAEMON_ATTEMPT_LIMIT_MS`. It is a
 * dropped attempt and never a refused one: the daemon may have applied it. */
export class LegionDaemonNoAnswerError extends Error {
  constructor(
    readonly label: string,
    readonly limitMs: number
  ) {
    super(
      `${label}: the Legion daemon took the connection and did not answer within ${limitMs / 60_000} minutes`
    );
    this.name = "LegionDaemonNoAnswerError";
  }
}

/** How this client reads each end Bun's fetch reports by a code: `refused` never reached the
 * daemon — the connection was refused, the host does not resolve, or it cannot be reached, so
 * nothing was sent and nothing was applied — and `dropped` may have reached it, a connection
 * reset, closed or timed out after it was made telling a landed attempt from a refused one no
 * better than silence does. A code this table does not name is no end of the daemon's serving.
 */
const DAEMON_END_BY_CODE: Readonly<Record<string, "refused" | "dropped">> = {
  ConnectionClosed: "dropped",
  ConnectionRefused: "refused",
  ConnectionTimeout: "dropped",
  EAI_AGAIN: "refused",
  ECONNREFUSED: "refused",
  ECONNRESET: "dropped",
  EHOSTUNREACH: "refused",
  ENETUNREACH: "refused",
  ENOTFOUND: "refused",
  EPIPE: "dropped",
  ETIMEDOUT: "dropped",
};

/** Why a request got no answer from the daemon, which is this client's whole reading of "the
 * daemon is not serving" (R1): `refused` never reached it (nothing was sent); `dropped` may have
 * reached it (a connection reset, closed or timed out before a response, an attempt the daemon took
 * and never answered, or a 502, 503 or 504 from a proxy in front of it, whose body is not the
 * daemon's refusal shape). Undefined for an answer the daemon gave, which is never waited out: a
 * 4xx above all, an uncoded 500, and the daemon's own coded 500 (`FACTS_UNAVAILABLE`,
 * `DISPATCH_UNAVAILABLE`) and 502 (`DISPATCH_FAILED`), each of which says itself what the caller
 * does next. */
export function daemonUnreachable(error: unknown): "refused" | "dropped" | undefined {
  if (error instanceof LegionDaemonNoAnswerError) return "dropped";
  if (error instanceof LegionDaemonApiError) {
    return (error.status === 502 || error.status === 503 || error.status === 504) &&
      error.code === undefined
      ? "dropped"
      : undefined;
  }
  if (!(error instanceof Error)) return undefined;
  const code = "code" in error && typeof error.code === "string" ? error.code : undefined;
  const end = code === undefined ? undefined : DAEMON_END_BY_CODE[code];
  if (end !== undefined) return end;
  return error instanceof TypeError &&
    (error.message === "Failed to fetch" || error.message === "fetch failed")
    ? "dropped"
    : undefined;
}

/** The wait before the next attempt: a timer the caller's abort ends early. It is the default
 * of `DaemonRestartRetry.sleep`, which a test replaces with a clock of its own. */
function waitFor(ms: number, abort?: AbortSignal): Promise<void> {
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
}

/** One attempt's own limit: it answers once `ms` have passed, and answers never once the
 * attempt has settled or stepped outside the daemon, since time that is not the daemon's is not
 * overdue. */
function limitOneAttempt(ms: number, answered: AbortSignal): Promise<void> {
  const overdue = Promise.withResolvers<void>();
  const timer = setTimeout(overdue.resolve, ms);
  answered.addEventListener("abort", () => clearTimeout(timer), { once: true });
  return overdue.promise;
}

/** What `oneAttempt` answers when the limit ran out before the daemon did. */
const noAnswer = Symbol("no answer from the daemon");

/** What `oneAttempt` answers when the caller's abort came and the attempt still had no end. */
const callerAborted = Symbol("the caller aborted the call");

/**
 * How an attempt runs a step of its own that is no request to the daemon: `handoff_complete`'s
 * report read in the pane (its jj and its Dispatch request). `step` runs outside the attempt's
 * limit, outside the record of the call's wait for the daemon and outside the call's budget for
 * that wait, and the limit starts again, whole, once the step is over: a pane that is slow to
 * answer — a jj waiting on a working-copy lock — is not a daemon that stopped serving, and the
 * worker's own stall watch sees it as it sees any other step of the call. A failure of `step` is
 * its own answer and is never waited out, whatever it reads as (`daemonUnreachable`). The
 * caller's abort still reaches it through the attempt's signal.
 */
export type OutsideTheDaemon = <S>(step: () => Promise<S>) => Promise<S>;

/** The part of a call that is time on the daemon, which a step outside it pauses and resumes
 * (`OutsideTheDaemon`): the record of the call's wait (`src/daemon-wait.ts`) and its budget. */
interface TimeOnTheDaemon {
  readonly paused: () => void;
  readonly resumed: () => void;
}

/**
 * One attempt, bounded at `DAEMON_ATTEMPT_LIMIT_MS` and by the caller's own abort (`cancel`). A
 * daemon that takes the connection and never answers is a daemon that is not serving, so the
 * attempt is aborted — which closes its connection before the next attempt is sent, so the
 * daemon never has two of them in flight and the registration it applies last is the one this
 * client read — and the wait tries again. The limit runs only while the attempt is on the
 * daemon: a step it runs through `outside` stops the limit and starts it again, whole, once it is
 * over, and a failure of that step is put in `ownFailures`, which the wait passes on as it is. The
 * caller's abort ends the attempt at once, and the call with it: the attempt's signal is aborted
 * the moment the caller's is, and an attempt that has not ended by the next turn of the event
 * loop is answered as aborted, since an attempt that does not end on its signal must not hold the
 * call. An end the attempt had already reached, or reached through that abort — a refused
 * connection, the daemon's answer — is its own.
 *
 * An attempt given up on in either way can no longer advance: `call` hands `attempt` to every
 * request it makes, and the client refuses to send a request, or to hand on an answer, once that
 * signal is aborted (`DaemonRequestOptions`). So a request of it that answers late mints no grant
 * and posts nothing after it, whatever it answers.
 *
 * `Promise.race` puts a handler on the attempt, so a call that fails after it was given up on is
 * answered here rather than left as an unhandled rejection.
 */
async function oneAttempt<T>(
  label: string,
  call: (attempt: AbortSignal, outside: OutsideTheDaemon) => Promise<T>,
  limit: (ms: number, answered: AbortSignal) => Promise<void>,
  cancel: AbortSignal | undefined,
  onTheDaemon: TimeOnTheDaemon,
  ownFailures: Set<unknown>
): Promise<T> {
  const attempt = new AbortController();
  const aborted = new Error(
    `${label}: the call was aborted while an attempt waited for the daemon's answer; that attempt may have been applied, so read the record before calling again`
  );
  const cancelled = Promise.withResolvers<typeof callerAborted>();
  const onCancel = () => {
    attempt.abort(aborted);
    setImmediate(() => cancelled.resolve(callerAborted));
  };
  cancel?.addEventListener("abort", onCancel, { once: true });
  const overdue = Promise.withResolvers<typeof noAnswer>();
  let settled = false;
  let stepping = false;
  // The limit now running, ended when the attempt settles or steps outside the daemon; a limit
  // that answers once it is ended answers nothing.
  let running = new AbortController();
  const startLimit = () => {
    const answered = new AbortController();
    running = answered;
    void limit(DAEMON_ATTEMPT_LIMIT_MS, answered.signal).then(() => {
      if (!answered.signal.aborted) overdue.resolve(noAnswer);
    });
  };
  const outside: OutsideTheDaemon = async (step) => {
    running.abort();
    stepping = true;
    onTheDaemon.paused();
    try {
      return await step();
    } catch (error) {
      ownFailures.add(error);
      throw error;
    } finally {
      stepping = false;
      // A step that ends after the attempt settled (the caller's abort came first) starts no
      // limit and records no wait: the call it belonged to is over.
      if (!settled) {
        onTheDaemon.resumed();
        startLimit();
      }
    }
  };
  let outcome: { readonly value: T } | typeof noAnswer | typeof callerAborted;
  try {
    // The call is begun before its limit, as the limit times what the call sent; a call that
    // stepped outside the daemon at once starts its limit when that step is over.
    const answer = call(attempt.signal, outside).then((value) => ({ value }));
    if (!stepping) startLimit();
    outcome = await Promise.race([answer, overdue.promise, cancelled.promise]);
  } catch (error) {
    // A request the caller's abort ended rejects with the abort, whose reason is `aborted`; Bun's
    // fetch hands that reason on, and a transport that names it an AbortError is the same end.
    throw attempt.signal.reason === aborted &&
      (error === aborted || (error instanceof Error && error.name === "AbortError"))
      ? aborted
      : error;
  } finally {
    settled = true;
    running.abort();
    cancel?.removeEventListener("abort", onCancel);
  }
  if (outcome === noAnswer) {
    const overdueError = new LegionDaemonNoAnswerError(label, DAEMON_ATTEMPT_LIMIT_MS);
    attempt.abort(overdueError);
    throw overdueError;
  }
  if (outcome === callerAborted) throw aborted;
  return outcome.value;
}

/**
 * Runs `call` until it gets the daemon's answer, retrying with capped exponential backoff while
 * the daemon is not serving (`daemonUnreachable`), for up to `DAEMON_RESTART_BUDGET_MS` or until
 * `signal` aborts; an abort sends no further attempt and ends the one in flight. One attempt that
 * the daemon takes and does not answer within `DAEMON_ATTEMPT_LIMIT_MS` is ended and made again
 * (`oneAttempt`), and `call` is handed that attempt's own signal to pass to every request it
 * makes, so an attempt given up on mints and posts nothing more. `call` is also handed `outside`,
 * which runs a step of the attempt that is no request to the daemon off the daemon's clock: off
 * the attempt's limit, the record of the call's wait and the budget (`OutsideTheDaemon`).
 *
 * Only a call that the daemon applies at most once however often it arrives may run through it:
 * `claims/register` (a repeat from the recorded session is reissued a secret, and the client
 * holds the one the daemon applied last), `claims/ready` (a repeat readies the claim that is
 * ready), `handoff_complete` (a repeat
 * is answered HANDOFF_ALREADY_RECORDED, or, once the completion moved the issue, refused as no
 * longer the role's phase), `sign_off` (a repeat finds the issue out of accepting and changes
 * nothing), `register_gate` (its fact is keyed by the gate's issue, generation, document,
 * version and Dispatch sequence; a repeat after the sequence moved is a new fact, which the gate
 * takes as a re-registration of the same document and tells no one again) and
 * `claims/turn-outcome` (one outcome per claim, generation and sequence, so a repeat writes the
 * cell its first attempt wrote). `call` is the whole
 * operation, grant mint included, since grants live in the daemon's memory and do not survive
 * its restart. `onRetrying` is told once. A daemon refusal after a dropped attempt is told that
 * the dropped one may have been applied.
 */
export async function retryWhileDaemonRestarts<T>(
  label: string,
  call: (attempt: AbortSignal, outside: OutsideTheDaemon) => Promise<T>,
  options: DaemonRestartRetry = {}
): Promise<T> {
  const { signal } = options;
  const now = options.now ?? Date.now;
  const sleep = options.sleep ?? waitFor;
  const limit = options.attemptLimit ?? limitOneAttempt;
  let deadline = now() + DAEMON_RESTART_BUDGET_MS;
  let delay = DAEMON_RESTART_FIRST_DELAY_MS;
  let told = false;
  let dropped = false;
  let lastFailure: unknown;
  const ownFailures = new Set<unknown>();
  /** The record of this call's time on the daemon, which the worker's own stall watch reads so
   * that no outage is counted against the tool call that waited it out (`src/daemon-wait.ts`).
   * It is opened at the call's first request, since a daemon that takes the connection and does
   * not answer is not serving from that moment, though nothing says so until the attempt's limit
   * five minutes later; and only for a wait a tool call owns, so a wait no tool call owns records
   * nothing. It is one record for the whole call, and a step outside the daemon is marked inside
   * it: that step's time is the call's own, which the stall watch counts. */
  const waited =
    options.toolCallId === undefined ? undefined : daemonWaitBegan(options.toolCallId, now());
  // The budget is for time on the daemon, so a step outside it moves the deadline on by as long.
  let pausedAt = 0;
  const onTheDaemon: TimeOnTheDaemon = {
    paused: () => {
      pausedAt = now();
      waited?.paused(pausedAt);
    },
    resumed: () => {
      const resumedAt = now();
      deadline += resumedAt - pausedAt;
      waited?.resumed(resumedAt);
    },
  };
  try {
    for (;;) {
      // An abort sends nothing more: a write the agent cancelled before it was sent is never
      // applied.
      if (signal?.aborted) {
        throw new Error(
          lastFailure === undefined
            ? `${label}: the call was aborted before it was sent; nothing was sent`
            : `${label}: the Legion daemon was unreachable (${messageFor(lastFailure)}) and the call was aborted${dropped ? "; an attempt lost its connection before the daemon answered and may have been applied, so read the record before calling again" : "; call it again once the daemon is back"}`
        );
      }
      try {
        return await oneAttempt(label, call, limit, signal, onTheDaemon, ownFailures);
      } catch (error) {
        // A step outside the daemon answered for itself: its failure is the call's, never a
        // daemon to wait for.
        if (ownFailures.has(error)) throw error;
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
  } finally {
    waited?.ended(now());
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
   * is served the shape of the pod the daemon places it in. A request whose `signal` is aborted
   * is not sent, and an answer that arrives once it is aborted is not handed on: each throws the
   * signal's reason (`DaemonRequestOptions`). The second check is what holds the fence when a
   * request's own abort did not end it, as when its answer had already arrived. */
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
    signal?.throwIfAborted();
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
    signal?.throwIfAborted();
    const daemonContract = statedContract(response.headers);
    if (response.ok) return { status: response.status, text, daemonContract };
    const refusal = parseBody(LegionErrorResponse, text);
    if ("value" in refusal) {
      throw new LegionDaemonApiError(
        method,
        path,
        response.status,
        refusal.value.code,
        refusal.value.error
      );
    }
    // A body that is not the daemon's refusal was written by something in its place, which can
    // echo the request back: it is named by its length alone (`unreadableBody`), never quoted.
    throw new LegionDaemonApiError(
      method,
      path,
      response.status,
      undefined,
      `${unreadableBody(text)} is not the daemon's refusal shape (${refusal.problem}). ${contractStanding(pluginContract, daemonContract)}`
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
    body: Input,
    signal?: AbortSignal
  ): Promise<Output> => read("POST", path, response, request.parse(body) as object, signal);

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
    state: (options) =>
      read("GET", "/legion/v1/state", LegionStateResponse, undefined, options?.signal),
    record: async (issue, options) => {
      try {
        return await read(
          "GET",
          `/legion/v1/issues/${encodeURIComponent(issue)}/record`,
          LegionIssueRecordResponse,
          undefined,
          options?.signal
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
    health: (options) => read("GET", "/healthz", LegionHealthResponse, undefined, options?.signal),
    // The header alone, parsed from an answer whose body this build may be unable to read: a
    // reader that reports the two numbers side by side must reach the daemon's number whatever
    // its health body carries, so this parses no schema.
    daemonContract: async (options) =>
      (await call("GET", "/healthz", undefined, options?.signal)).daemonContract,
    register: (input, options) =>
      read("POST", "/legion/v1/claims/register", LegionRegisterResponse, input, options?.signal),
    registerController: (input) =>
      read("POST", "/legion/v1/claims/register", LegionControllerRegisterResponse, input),
    ready: (input, options) => acknowledge("/legion/v1/claims/ready", input, options?.signal),
    readyForUpdate: (input) => acknowledge("/legion/v1/claims/ready-for-update", input),
    exit: (input) => acknowledge("/legion/v1/claims/exit", input),
    turnOutcome: (input, options) =>
      post(
        "/legion/v1/claims/turn-outcome",
        LegionTurnOutcomeRequest,
        LegionTurnOutcomeResponse,
        input,
        options?.signal
      ),
    activity: (input) => acknowledge("/legion/v1/claims/activity", input),
    releaseRecords: (input) =>
      post(
        "/legion/v1/claims/release-records",
        LegionReleaseRecordsRequest,
        LegionReleaseRecordsResponse,
        input
      ),
    grant: (input, options) =>
      post("/legion/v1/grants", LegionGrantRequest, LegionGrantResponse, input, options?.signal),
    controllerGrant: (input) =>
      post("/legion/v1/grants", LegionControllerGrantRequest, LegionGrantResponse, input),
    handoffComplete: (input, options) =>
      post(
        "/legion/v1/handoff/complete",
        LegionHandoffCompleteRequest,
        LegionHandoffCompleteResponse,
        input,
        options?.signal
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
    relaunch: (input) =>
      post("/legion/v1/claims/relaunch", LegionRelaunchRequest, LegionRelaunchResponse, input),
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
    gateRegister: (input, options) =>
      post(
        "/legion/v1/gates/register",
        LegionGateRegisterRequest,
        LegionEmptyResponse,
        input,
        options?.signal
      ),
    waveRelease: (input) =>
      post("/legion/v1/waves/release", LegionWaveReleaseRequest, LegionWaveReleaseResponse, input),
    phaseBackward: (input) =>
      post("/legion/v1/phase/backward", LegionPhaseBackwardRequest, LegionEmptyResponse, input),
    phaseRetry: (input) =>
      post("/legion/v1/phase/retry", LegionPhaseRetryRequest, LegionEmptyResponse, input),
    signOff: (input, options) =>
      post("/legion/v1/signoff", LegionSignOffRequest, LegionEmptyResponse, input, options?.signal),
    rootClose: (input) =>
      post("/legion/v1/roots/close", LegionRootCloseRequest, LegionEmptyResponse, input),
    childPark: (input) =>
      post("/legion/v1/children/park", LegionChildRequest, LegionEmptyResponse, input),
    childRerun: (input) =>
      post("/legion/v1/children/rerun", LegionChildRequest, LegionEmptyResponse, input),
  };
}
