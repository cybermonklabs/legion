import { randomUUID } from "node:crypto";
import path from "node:path";
import { activeDispatchSettings } from "@legion/envoy-client/dispatch-config";
import { resolveIssueDocumentId } from "@legion/envoy-client/dispatch-execute";
import { DispatchClient } from "@legion/envoy-client/dispatch-http";
import { sessionDirectory as dispatchSessionDirectory } from "@legion/envoy-client/dispatch-session-directory";
import { writeSessionTitle } from "@legion/envoy-client/dispatch-session-state";
import { messageFor } from "@legion/envoy-client/errors";
import {
  sessionConfigFile,
  sessionDispatchSettings,
  sessionEnvoySettings,
} from "@legion/envoy-client/session-settings";
import { createEnvoyClient } from "@legion/envoy-client/transport";
import {
  BoundedStepFailure,
  boundedStep,
  type ClaimBootBounds,
  createLegionDaemonClient,
  type LegionDaemonClient,
  type StepBound,
} from "@legion/pi-shared/daemon-client";
import { currentSessionID } from "@legion/pi-shared/dispatch-session-id";
import { nextFallbackModel } from "@legion/pi-shared/fallback-chain";
import {
  fallbackCooldownLimits,
  LEGION_FALLBACK_ENTRY,
  recordedFallbackCooldown,
  restoreFallbackCooldown,
} from "@legion/pi-shared/fallback-cooldown";
import { findPushedBranchHead, paneHandoffReport } from "@legion/pi-shared/handoff-record";
import { matchInjectedUserTurn } from "@legion/pi-shared/injected-user-turns";
import {
  ENVOY_PLUGIN_INTERFACE_VERSION,
  LEGACY_LEGION_LOADED_KEY,
  LEGION_PLUGIN_LOADED_KEY,
  readEnvoyPluginInterface,
} from "@legion/pi-shared/interface";
import { claimIdentityOf, claimIdentityWords } from "@legion/pi-shared/legion-claim-core";
import {
  classifySession,
  daemonLaunchedAgentProcess,
  type LegionSessionKind,
  requiredEnvironment,
} from "@legion/pi-shared/legion-session";
import { createPhaseReminder, type EnvoyPublish } from "@legion/pi-shared/phase-reminder";
import {
  assistantRanTool,
  assistantText,
  inboundKind,
  PHASE_STALL_ENTRY,
  restorePhaseStallEntry,
} from "@legion/pi-shared/phase-stall";
import type { CommandContext, PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import { legionRoleClaimBridge } from "@legion/pi-shared/role-claim-bridge";
import {
  asyncJobKinds,
  type SessionActivityReport,
  sameSessionActivity,
  sessionActivity,
  sessionActivityRequest,
} from "@legion/pi-shared/session-activity";
import { stallLimits } from "@legion/pi-shared/stall-limits";
import {
  createStallWatchdog,
  type StallWatchdog,
  type StallWatchdogProcesses,
} from "@legion/pi-shared/stall-watchdog";
import { clearStartupTurn } from "@legion/pi-shared/startup-turn";
import { subagentSessionCheck } from "@legion/pi-shared/subagent-session";
import { logger } from "@oh-my-pi/pi-utils";
import {
  type CapabilityBudgets,
  type CapabilityHost,
  measureCapabilities,
  ompCapabilityHost,
} from "../src/capability-report";
import {
  CAPABILITY_SEND_TIMEOUT_MS,
  type CapabilitySchedule,
  createCapabilitySchedule,
} from "../src/capability-schedule";
import { createClaimSession } from "../src/claim-session";
import { createControllerSession } from "../src/controller-session";
import { createModelFallback } from "../src/model-fallback";
import { applySessionTitle, legionSessionTitle } from "../src/session-title";
import { daemonLaunchedController } from "../src/slack-channels";
import { createStallAborter } from "../src/stall-abort";
import { createStallTeller } from "../src/stall-evidence";
import { createStallExits } from "../src/stall-exits";
import { createStallProcesses, type StallProcessesDeps } from "../src/stall-recovery";
import { createLegionTool } from "../src/tools";
import {
  branchHoldsWorkspaceRecreatedNotice,
  type WorkspaceRecreatedNotice,
  withoutRepeatedWorkspaceRecreatedNotice,
  workspaceRecreatedMessage,
  workspaceRecreatedNotice,
} from "../src/workspace-recreated";

/**
 * Oh My Pi's own bound on one extension handler, measured in this pod on 2026-11-11: a
 * `session_start` held past it is aborted — `{"type":"extension_error",…,"error":"handler timed
 * out after 30000ms"}`, the abort firing at 29 998 ms and at 30 003 ms in two runs — the session
 * runs on, and `pi.getActiveTools()` holds no `legion`. This entry never lets its own handler
 * reach it: a role whose process kept the `envoy_*` tools and no `legion` tool runs a whole phase
 * unable to report its work (dispatch://INFRA-2883).
 */
export const OMP_HANDLER_BOUND_MS = 30_000;

/**
 * The bound this entry holds the whole of its own `session_start` to, from its first statement
 * through the one that activates the tool, and the same for `/legion-claim-controller`, the one
 * other path that registers it. It sits between the claim boot's whole budget
 * (`CLAIM_BOOT_BUDGET_MS`, 20 000 ms, `@legion/pi-shared/daemon-client`) and Oh My Pi's own
 * {@link OMP_HANDLER_BOUND_MS}: above the boot, so a boot that spends its budget still writes its
 * own line and ends the process itself; below the harness, so a handler that would outlive its
 * bound decides its own ending — one line naming the step, then the process ended for the daemon
 * to launch again — rather than being aborted into the session this issue is about.
 */
export const LEGION_SESSION_START_BOUND_MS = 25_000;

// The bound above, as a test sets it. Production never sets this, so both handlers keep the real
// 25 s; a test sets a short one, since no row can wait out 25 seconds.
let sessionStartBoundMs = LEGION_SESSION_START_BOUND_MS;
export function setLegionSessionStartBoundForTests(ms: number | undefined): void {
  sessionStartBoundMs = ms ?? LEGION_SESSION_START_BOUND_MS;
}

// The two bounds the claim boot and the controller's own registration run under
// (`CLAIM_BOOT_ATTEMPT_TIMEOUT_MS` and `CLAIM_BOOT_BUDGET_MS`). Production sets none and takes
// those two; a test sets its own, since no row can wait out the production budget.
let claimBootBounds: ClaimBootBounds | undefined;
export function setLegionClaimBootBoundsForTests(bounds: ClaimBootBounds | undefined): void {
  claimBootBounds = bounds;
}

/** `error` as the one line reports it: the step it failed at, and why. */
function stepFailure(error: unknown): { readonly step: string; readonly why: string } {
  return error instanceof BoundedStepFailure
    ? { step: error.step, why: messageFor(error.failure) }
    : { step: "an unnamed step", why: messageFor(error) };
}

/** One handler's bound while it runs, as `boundedStep` reads it
 * (`@legion/pi-shared/daemon-client`, where the claim boot's budget reads the same way).
 * Undefined is a process this entry ends nothing in (a session a person started): a step runs
 * unbounded there, as it did before this cover, and only its failure is named, for the caller
 * that hands it back as the command's own error. */
function handlerStepBound(boundMs: number): StepBound {
  const deadline = Date.now() + boundMs;
  return {
    remainingMs: () => deadline - Date.now(),
    named: `the handler's bound of ${boundMs} ms`,
  };
}

// Fatal bootstrap failures call this instead of `process.exit` directly, so a
// test can substitute a throwing stand-in without killing the test runner. The stall watch's
// last step reaches this same seam, since that exit is the one the daemon reads as a death.
// Production callers never override it.
let exitProcess: (code: number) => never = (code) => process.exit(code) as never;
export function setLegionBootstrapExitForTests(hook: (code: number) => never): void {
  exitProcess = hook;
}

// The process table the stall watch reads its evidence from and ends a stuck call's processes
// through (`src/stall-recovery.ts`): this machine's own `/proc` and `process.kill`. A test
// substitutes a fake, since the production one reads the live tree of whatever machine the test
// runs on and ends what it finds there.
let stallProcessHost: (deps: StallProcessesDeps) => StallWatchdogProcesses = createStallProcesses;
export function setLegionStallProcessesForTests(
  host: ((deps: StallProcessesDeps) => StallWatchdogProcesses) | undefined
): void {
  stallProcessHost = host ?? createStallProcesses;
}

// The host the claim session's capability report measures through: Oh My Pi's own modules and
// the pane's `dispatch` and `gh`. A test substitutes a fake, since the production host reaches
// modules that exist only inside Oh My Pi's bundle and runs the pane's commands.
let capabilityHost: () => CapabilityHost = ompCapabilityHost;
export function setLegionCapabilityHostForTests(host: () => CapabilityHost): void {
  capabilityHost = host;
}

// The budgets the capability measurement runs under. Production never sets this, so
// `measureCapabilities` keeps its own 8 s per check and 10 s per report; a test sets a short pair,
// since one row that fails on its own budget would otherwise cost 8 s, past Bun's 5 s default for
// a single test.
let capabilityBudgets: CapabilityBudgets | undefined;
export function setLegionCapabilityBudgetsForTests(budgets: CapabilityBudgets | undefined): void {
  capabilityBudgets = budgets;
}

// The bound on one `claims/ready` the capability schedule sends. Production never sets this, so
// the send keeps `CAPABILITY_SEND_TIMEOUT_MS`, the real 10 s; a test sets a short one, since a
// send the daemon never answers would otherwise hold its tick for 10 s, past Bun's 5 s default
// for a single test.
let capabilitySendTimeoutMs = CAPABILITY_SEND_TIMEOUT_MS;
export function setLegionCapabilitySendTimeoutForTests(ms: number | undefined): void {
  capabilitySendTimeoutMs = ms ?? CAPABILITY_SEND_TIMEOUT_MS;
}

async function persistedTranscript(
  context: CommandContext | SessionContext
): Promise<{ readonly sessionFile: string; readonly agentId: string }> {
  await context.sessionManager.ensureOnDisk();
  const sessionFile = context.sessionManager.getSessionFile();
  if (!sessionFile?.endsWith(".jsonl")) {
    throw new Error("Legion session must have a persisted transcript");
  }
  const agentId = path.basename(sessionFile, ".jsonl");
  if (!agentId) throw new Error("Legion session transcript has no agent id");
  return { sessionFile, agentId };
}

/**
 * Why this Legion entry cannot run in this process, or undefined when it can. Legion claims roles
 * and reads deliveries through the interface the Envoy plugin publishes
 * (`@legion/pi-shared/interface`), so a process without `@sjawhar/pi-envoy`, or with one that
 * speaks another interface version, has nothing to claim through; and the pre-split
 * `@sjawhar/pi-legion-envoy` still installed beside this plugin would run a second Legion entry
 * against the same pane. The sentence names the remedy, since the daemon's boot log and the
 * operator's own session are where it lands.
 */
function envoyPluginRefusal(): string | undefined {
  const legacy = (globalThis as Record<symbol, unknown>)[LEGACY_LEGION_LOADED_KEY];
  if (legacy !== undefined) {
    const where = typeof legacy === "string" ? ` from ${legacy}` : "";
    return `Legion plugin at ${import.meta.url} found the pre-split @sjawhar/pi-legion-envoy loaded${where}; uninstall @sjawhar/pi-legion-envoy (omp plugin uninstall @sjawhar/pi-legion-envoy) and keep @sjawhar/pi-envoy beside @sjawhar/pi-legion`;
  }
  const reading = readEnvoyPluginInterface();
  switch (reading.kind) {
    case "present":
      return undefined;
    case "absent":
      return `Legion plugin at ${import.meta.url} needs @sjawhar/pi-envoy at interface version ${ENVOY_PLUGIN_INTERFACE_VERSION}, found none; install @sjawhar/pi-envoy beside @sjawhar/pi-legion`;
    case "mismatch":
      return `Legion plugin at ${import.meta.url} needs @sjawhar/pi-envoy at interface version ${reading.expected}, found version ${reading.found} from ${reading.from}; install the @sjawhar/pi-envoy released with this @sjawhar/pi-legion`;
  }
}

export default function legionExtension(pi: PiApi): void {
  // One instance per session (a `task` subagent gets its own); the id tells them apart in the log.
  const instance = randomUUID().slice(0, 8);
  logger.debug("extension instance loaded", { extension: import.meta.url, instance });
  // Read by the daemon's boot gate to prove this extension actually loaded from an ambient
  // installed-plugin discovery -- not just that a manifest file exists, which stays true even when
  // the plugin is disabled or unregistered in OMP's own plugin registry -- and which interface
  // version it speaks.
  (globalThis as Record<symbol, unknown>)[LEGION_PLUGIN_LOADED_KEY] = {
    from: import.meta.url,
    envoyInterface: ENVOY_PLUGIN_INTERFACE_VERSION,
  };
  // Whether this is the daemon-launched controller, whose Slack subscriptions its role owns: the
  // Envoy entry's recovery of a resumed session's registered interests then drops every Slack
  // topic before it restores the rest. Set here, at load, because that recovery runs in the Envoy
  // entry's session_start, before this entry's claim.
  if (daemonLaunchedController(process.env)) {
    legionRoleClaimBridge().slackRoleOwned = true;
  }

  // Gates session_start and the session-change re-claim below, one call per hook; its settled
  // answer is kept, so it runs once per session.
  const checkSubagentSession = subagentSessionCheck();

  // The endless phase reminder (src/phase-stall.ts, src/phase-reminder.ts). It runs only in a
  // session holding a claim for its own id with a phase role, so never in an architect (a root or
  // a sub-architect), the controller (whose claim lives in controllerSession), a session with no
  // Legion environment, or a `task` subagent (whose instance returns at checkSubagentSession
  // before any capability exists). Restored from the transcript at session_start and appended to
  // it on every change.
  //
  // The runner's timers and its record check run outside every hook, so the pane's own session
  // context is kept here (`paneContext`, set at session_start and after every session change):
  // one Oh My Pi process holds one claim for its whole life, and this is the session that claim
  // is on.
  let paneContext: SessionContext | undefined;
  const phaseWorkerSession = (context: SessionContext): boolean => {
    const role = claimSession.capability(context.sessionManager.getSessionId())?.role;
    return role !== undefined && role !== "architect";
  };
  // Every Legion role falls back down Oh My Pi's model chain when a provider's content filter
  // refuses its turn (INFRA-956 R1), the architect and the controller included, so the model
  // fallback gates on the session holding a Legion claim of any kind rather than on
  // `phaseWorkerSession`, which excludes an architect and serves the phase reminder alone.
  const legionClaimSession = (context: CommandContext | SessionContext): boolean => {
    const sessionID = context.sessionManager.getSessionId();
    return (
      claimSession.capability(sessionID) !== undefined ||
      controllerSession.isClaimedSession(sessionID)
    );
  };
  // A `task` subagent walks its own chain and must not move the pane's model, and a session with
  // no claim is nobody's Legion role; the subagent check is the same one session_start asks, so
  // it is settled by the time any of these hooks runs.
  const modelFallbackSession = async (context: SessionContext): Promise<boolean> =>
    !(await checkSubagentSession(context)) && legionClaimSession(context);
  // The claim this pane holds, read live from whatever session the pane is on: the reminder's
  // notice, the stall watch's evidence and its owner's notice each name it.
  const paneClaim = () =>
    paneContext === undefined
      ? undefined
      : claimSession.capability(paneContext.sessionManager.getSessionId());
  // The numbers both watches of this entry run on (`@legion/pi-shared/stall-limits`), read once
  // at load: the reminder's thresholds, and the stall watch's grace over a call's own declared
  // deadline, its bound for a call no deadline judges, and the gap between its steps.
  const limits = stallLimits(process.env, logger, process.env.LEGION_ROLE);
  // The Envoy listener this entry's own notices go to — the architect's reminder notice, and the
  // stall watch's notice of a stuck call — built once and only when a notice is due: this entry
  // reaches no listener of its own otherwise. Envoy off for the session leaves it undefined, and
  // the reminder keeps coming without a notice.
  let noticePublisher: { readonly publish: EnvoyPublish | undefined } | undefined;
  const publisher = (): EnvoyPublish | undefined => {
    if (noticePublisher === undefined) {
      const envoy = sessionEnvoySettings(sessionConfigFile("omp", process.env), process.env, {
        cwd: process.cwd(),
        nats: false,
        session: currentSessionID,
      });
      noticePublisher = {
        publish: envoy.enabled
          ? createEnvoyClient({
              baseUrl: envoy.url,
              credential: envoy.credential,
              fetch,
            }).publish
          : undefined,
      };
    }
    return noticePublisher.publish;
  };
  const phaseReminder = createPhaseReminder({
    sendReminder: (reminder) =>
      pi.sendMessage(
        { customType: "legion-phase-reminder", content: reminder, display: true },
        { deliverAs: "steer", triggerTurn: true }
      ),
    armTimer: (callback, delayMs) => paneContext?.setTimeout(callback, delayMs),
    saveEntry: (entry) => pi.appendEntry(PHASE_STALL_ENTRY, entry),
    claim: paneClaim,
    readState: (signal) => roleDaemon().state({ signal }),
    publisher,
    project: process.env.LEGION_PROJECT,
    limits,
    log: logger,
  });

  // The model fallback (src/model-fallback.ts) on the cooldown machine
  // (@legion/pi-shared/fallback-cooldown): the two waits come from this worker's own environment
  // with the specification's defaults, as the stall limits do, and the state is kept in the
  // transcript so a relaunched worker comes back with its due time and its doubling levels.
  const modelFallback = createModelFallback({
    setModel: async (model) => (await pi.setModel?.(model)) === true,
    saveState: (state) => pi.appendEntry(LEGION_FALLBACK_ENTRY, recordedFallbackCooldown(state)),
    now: () => Date.now(),
    limits: fallbackCooldownLimits(process.env, logger),
    nextModel: nextFallbackModel,
    log: logger,
  });

  // The fallback state of whatever session the pane is on: the record this plugin saved
  // (`restoreFallbackCooldown`), and the fallback the branch itself may already sit on. Read
  // after every claim this process registers — at session_start, after a session change, and
  // from each of the controller's two claim commands below, which are the only other places one
  // is registered — and only for a session that claim is on: the restore's own save appends an
  // entry to that session's transcript, and a person's own session, or a subagent's, that
  // happens to sit on a host fallback is no Legion role's to arm or to write into.
  const restoreModelFallback = (context: CommandContext | SessionContext): void => {
    if (!legionClaimSession(context)) return;
    modelFallback.restore(
      restoreFallbackCooldown(context.sessionManager.getBranch?.() ?? []),
      context
    );
  };

  /**
   * INFRA-945: what this pane is doing, which the daemon reads to decide whether its issue pod
   * may be released. A report is sent at every settle, change or no change — the daemon releases
   * no claim whose last report predates its last turn, so a quiet turn that sent nothing would
   * hold the pod for ever — and between turns only when the report's content changes.
   *
   * Oh My Pi fires no event for background work, so what it is running is read rather than
   * waited for: `SESSION_ACTIVITY_PERIOD_MS` apart, the host's own managed interval reads
   * `ctx.getAsyncJobSnapshot()`, which is an in-memory look at this session's own job manager,
   * and compares the report it builds with the last one sent. Nothing leaves the pane unless
   * that content changed, so the cost between turns is one object comparison, and five seconds
   * bounds how stale the daemon's view of a job that just started can be against a release rule
   * measured in minutes.
   */
  const SESSION_ACTIVITY_PERIOD_MS = 5_000;
  let lastActivity: SessionActivityReport | undefined;
  const reportActivity = async (
    context: SessionContext,
    options: { readonly onlyWhenChanged: boolean }
  ): Promise<void> => {
    const claim = claimSession.capability(context.sessionManager.getSessionId());
    if (claim === undefined) return;
    // An architect arms no phase reminder, so it waits on no reminder of its own; what it
    // reports is its background work.
    const report = sessionActivity(
      asyncJobKinds(context.getAsyncJobSnapshot?.() ?? null),
      phaseWorkerSession(context) ? phaseReminder.phaseWake() : { nextReminderAt: null }
    );
    if (
      options.onlyWhenChanged &&
      lastActivity !== undefined &&
      sameSessionActivity(lastActivity, report)
    ) {
      return;
    }
    const request = sessionActivityRequest({
      project: process.env.LEGION_PROJECT,
      claim,
      report,
    });
    if (request === undefined) return;
    lastActivity = report;
    try {
      await roleDaemon().activity(request);
    } catch (error) {
      // The daemon did not take it, so it is not what the daemon holds: the next reading sends
      // again whether or not its content moved.
      lastActivity = undefined;
      logger.warn("legion: this session's activity report did not reach the daemon", {
        issue: claim.issue,
        error: messageFor(error),
      });
    }
  };

  let daemonClient: LegionDaemonClient | undefined;
  const roleDaemon = (): LegionDaemonClient => {
    daemonClient ??= createLegionDaemonClient(
      requiredEnvironment(process.env, "LEGION_DAEMON_URL")
    );
    return daemonClient;
  };

  // Whether this instance has already ended the process, and the one ending every part of this
  // entry takes: the claim boot's, the Envoy-plugin refusal's, the stall ladder's last step and
  // the cover below. In production `exitProcess` is `process.exit` and nothing runs after it; a
  // test's stand-in throws in its place, and that throw then travels up through whatever handler
  // was running — so the flag is what keeps the cover from reading an ending already taken, whose
  // line is already written, as a failure of its own.
  let processEnding = false;
  const endProcess: (code: number) => never = (code) => {
    processEnding = true;
    return exitProcess(code);
  };

  /**
   * The clock a handler that registers the `legion` tool runs under, started at its first
   * statement: {@link LEGION_SESSION_START_BOUND_MS} in the agent the daemon launched for a role,
   * and none in any other process, where this entry ends nothing and a step that never answers is
   * Oh My Pi's own to abort, exactly as before this cover (R1).
   */
  const handlerBound = (): StepBound | undefined => {
    if (!daemonLaunchedAgentProcess(process.env, process.argv)) return undefined;
    return handlerStepBound(sessionStartBoundMs);
  };

  /**
   * The one ending every failure and every overrun of the two handlers that register the `legion`
   * tool takes (R1, R3): one line naming who this process is — its role, issue and tree, and the
   * daemon it registers with (`claimIdentityWords`, the boot's own wording) — with the step and
   * the cause, then the process, so the daemon launches it again. A process that runs on past
   * such a failure keeps the `envoy_*` tools and no `legion` tool, which is a role that can claim,
   * report and hand off nothing, and which nothing else notices (dispatch://INFRA-2883).
   *
   * The line goes to Oh My Pi's log and to stderr. The log is under the role's own home, which
   * the pod does not keep; stderr is the pod's log, which an operator reads after the process is
   * gone, and it is where the boot's own line goes (`src/claim-session.ts`). The process this
   * ending is for is the daemon's `--mode rpc` agent, whose stderr no screen shares.
   *
   * Two endings are not this one. A process that is not the daemon's agent for a role — a session
   * a person started, the operator's own controller among them — is never ended here: the failure
   * is the command's own error, which Oh My Pi reports to whoever is reading it. And an ending
   * this handler has already taken, which wrote its own line, is handed on as it is.
   *
   * Which of the two it is, is `bound`: `handlerBound` answers one exactly for the processes
   * this ending is for, and both callers hold what it answered from their first statement. Read
   * again here it could differ — a step that rewrites this process's own environment is a case
   * the rows below cover — and then the handler would be bounded by one rule and ended by
   * another.
   */
  const endHandler = (handler: string, bound: StepBound | undefined, error: unknown): never => {
    if (processEnding) throw error;
    if (bound === undefined) {
      throw error instanceof BoundedStepFailure ? error.failure : error;
    }
    const { step, why } = stepFailure(error);
    const line = `[legion] ${handler} failed at ${step}: ${why}; ${claimIdentityWords(claimIdentityOf(process.env))}; this session would run on with no legion tool, so exiting for the daemon to launch it again`;
    logger.error(line);
    console.error(line);
    return endProcess(1);
  };

  // A root architect's or phase worker's claim, and the operator-launched controller's.
  const claimSession = createClaimSession({
    daemon: roleDaemon,
    persistedTranscript,
    exitProcess: endProcess,
    bounds: () => claimBootBounds,
  });
  const controllerSession = createControllerSession({
    daemon: roleDaemon,
    persistedTranscript,
    pi,
    exitProcess: endProcess,
    bounds: () => claimBootBounds,
  });

  /**
   * INFRA-1274: the worker's watch on its own tool calls, and the ladder it drives when one of
   * them stops. The detector is `@legion/pi-shared/stall-watchdog`, which holds the open calls
   * and decides when a step runs; the four collaborators it reaches the machine, the listener and
   * this process through are `src/stall-evidence.ts` (the evidence and the owner's notice),
   * `src/stall-recovery.ts` (the tree, and the stuck call's own processes), `src/stall-abort.ts`
   * (the turn, ended, and the message that starts the work again) and `src/stall-exits.ts` (the
   * count that bounds the last step).
   *
   * **It is armed at `session_start`, for every Legion session**: a phase worker's, a root
   * architect's and the controller's alike. A `task` subagent's instance of this module returns
   * at `checkSubagentSession` before the arm, so it records nothing and sweeps nothing: a
   * subagent shares the worker's process, and the ladder's last step would end the parent with
   * it. The hooks below are registered at load, as every other hook of this entry is, and each
   * does nothing at all until a session arms the watch, which is what leaves a subagent's
   * instance recording nothing — and each returns for a subagent's own event where the host
   * hands it to this instance instead (`subagentCall`).
   */
  let stallWatch: StallWatchdog | undefined;
  /** The 5-second sweep, on the interval each session arms below. It is never awaited: a step of
   * the ladder can outlast the tick that armed it, and the detector takes one sweep at a time. */
  const stallSweep = (): void => {
    void stallWatch?.sweep();
  };
  const armStallWatch = (session: LegionSessionKind): void => {
    stallWatch = createStallWatchdog({
      now: Date.now,
      limits,
      // Live, as the activity report and the capability schedule read it; a host that answers
      // nothing is read as not idle, which is what a session running a stuck call is.
      isIdle: () => paneContext?.isIdle?.() ?? false,
      processes: stallProcessHost({
        // The transcript this session's tool runners name in `PI_SESSION_FILE`, which is the path
        // `persistedTranscript` put on disk at the boot below; read from the live session manager
        // rather than kept, so a session change carries the new one. A host that names no `.jsonl`
        // leaves every runner alone.
        sessionFile: () => {
          const file = paneContext?.sessionManager.getSessionFile();
          return file?.endsWith(".jsonl") === true ? file : undefined;
        },
        openCalls: () => stallWatch?.openCalls() ?? [],
        backgroundJobs: () => paneContext?.getAsyncJobSnapshot?.()?.running ?? [],
        log: logger,
      }),
      teller: createStallTeller({
        now: Date.now,
        session: () => session,
        claim: paneClaim,
        project: process.env.LEGION_PROJECT,
        readState: (signal) => roleDaemon().state({ signal }),
        publisher,
        appendEntry: (customType, data) => pi.appendEntry(customType, data),
        log: logger,
      }),
      aborter: createStallAborter({
        context: () => paneContext,
        // `pi`'s own method, called on `pi` as every other send of this entry is.
        sendMessage: (message, options) => pi.sendMessage(message, options),
      }),
      exits: createStallExits({
        stateDir: requiredEnvironment(process.env, "LEGION_STATE_DIR"),
        session,
        now: Date.now,
        log: logger,
      }),
      exitProcess: endProcess,
      log: logger,
    });
  };

  /** A `task` subagent's own event, which this watch is not about: the subagent shares the
   * worker's process, so a ladder run for one of its calls would end the worker. The host hands
   * a subagent's events a context that names its runner (`agent.kind`, as envoy.ts's own
   * `tool_call` hook reads it); a host that names no agent is the top-level session.
   *
   * **Seven handlers of this entry ask it**, and each is load-bearing: the two that open a call
   * (`tool_call`, `tool_execution_start`), the two that close one by id (`tool_execution_end`,
   * `tool_result`), and the three that close every call this watch holds at once (`turn_end`,
   * `agent_end`, and `message_end` — the last registered far below the others, with the
   * phase-stall handlers — whose aborted assistant message matches no id at all). A
   * subagent's event reaching any of the last three would carry off the worker's own open calls,
   * the `task` call running that subagent among them, and its stuck call would be judged by
   * nobody; through either of the two closing by id it would do the same wherever the provider
   * gives the two calls one id, which the ids being the provider's own leaves open. Each of the
   * seven is held by a case of `legion.test.ts` that is red when that guard alone is cut.
   *
   * **The guard on `message_end` and the close behind it hold each other**, which is the one
   * thing about that line no suite says: each half has a case that reds when it alone is cut,
   * but this guard's case, "a subagent's run cancelled on its own closes none of the worker's
   * calls", is about what the close does **not** do for a subagent, so it means something only
   * while the close is there. Take the close away with the case that holds it and this case
   * goes hollow rather than red. The same is written at that handler. */
  const subagentEvent = (context: SessionContext): boolean => context.agent?.kind === "sub";

  /**
   * **The `tool_call` handler records the call's arguments and returns at once.** It blocks
   * nothing, revises nothing and awaits nothing, which is what makes a handler on this event safe
   * to register: the host holds the whole turn for the handler's budget
   * (`extensionHandlers.toolCallTimeoutMs`, 30 s by default) and fails the call closed when the
   * handler throws or outruns it, both measured on omp/18.16.0 on 2026-11-07. This reverses the
   * decision this entry carried since LEGION-631 — that it registers no `tool_call` hook at all —
   * and the reversal is deliberate (dispatch://INFRA-1274/spec#b-r2): what is gone with that
   * decision is the pane rule and the per-command grant mint, which refused calls and rewrote
   * their arguments; this handler does neither, and nothing a pane runs is refused or minted for.
   *
   * `tool_call` is the one event the host fires before a call can run at all, so it is where the
   * arguments come from; the clock starts at `tool_execution_start` instead, so neither the queue
   * nor the approval gate is counted against a call.
   */
  pi.on("tool_call", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.callRequested(event);
  });
  pi.on("tool_execution_start", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.executionStarted(event);
  });
  // Both ends of a call close its record, the error end of a call the host failed closed before
  // it ran among them — and the end of a call the host backgrounded, whose command keeps
  // running. That call leaves this book while its process lives, and that is the record this
  // watch wants rather than a gap in it: the agent is no longer parked in the call, since the
  // host freed its turn and handed it the job, so the ladder's steps would end the processes of
  // a call nobody waits on and abort a turn that is working. What the agent does with the job
  // next is a call of its own with a clock of its own, and a session that goes quiet altogether
  // is the daemon's backstop to end. A plain (non-`pty`) shell command that does not return is
  // backgrounded about a minute in, or at once when a steering message reaches the turn; its
  // `tool_execution_end` carries no error and a job still running, this watch writes nothing for
  // it, and the command's process runs on (driven for the steered route in
  // `legion-stall-watchdog-omp.test.ts`, which is the one a case can hold: the call declares a
  // deadline the sweep would judge it by, and a case that waited out the host's own minute
  // would have to declare a deadline longer than itself and could then fail at nothing).
  // Each of these two closes is held by a case of `legion.test.ts` that is red when that one
  // alone is cut: a call ended by its execution's end, and a call whose result arrives with no
  // execution end of this session's. They are what keeps a finished call out of the ladder's
  // way while its turn goes on, which is most calls of a turn that makes several.
  pi.on("tool_execution_end", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.executionEnded(event);
  });
  pi.on("tool_result", async (event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.resultDelivered(event);
  });
  // The turn is over, so every call it held open is closed and no step runs for one of them, and
  // the turn's one abort goes with them so the next turn aborts for itself (`turnEnded` in
  // `pi-shared/src/stall-watchdog.ts`). How that turn ended is read nowhere here: an aborted
  // turn is readable from its last assistant message, which the `message_end` handler below
  // already hands the watch, and `turnEnded` closes what an aborted message would close and
  // clears what it would clear — so a call to `assistantMessage` here would be a second name
  // for the same close in the log and nothing else. A subagent's turn is not this session's and
  // closes none of its calls, the `task` call that is running it least of all; the case
  // "the turn's end closes every call it held open" holds this line.
  pi.on("turn_end", async (_event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.turnEnded();
  });
  // The run settled, or ended with the host going on: `turn_end` fires 3 ms before this for an
  // aborted run (measured), so this closes nothing that run left open; it is what closes a run
  // that ends with no turn end of its own. A run the host has something to continue with — a
  // steering delivery it took while the turn ran, a background job's follow-up — ends here too,
  // and the calls of that run are closed with every other. That is right because the host ends
  // every call of a run before its `turn_end` and `agent_end`: a tool round waits for every
  // call's `execute` and writes each call's `tool_execution_end` first, the error path
  // included, so no call of the run is still open here. It is not forced: an extension could
  // tell such an end apart — a background job's follow-up arrives with `willContinue` and
  // `awaitingAsyncWork` set, and `ctx.hasPendingMessages()` is true at a steered run's
  // non-terminal end — and the watch reads neither on purpose, since the turn after it is a
  // turn of its own with calls of its own. The cases "a run that settled with no turn end of
  // its own closes them at `agent_end`" and "a run that ends with the host going on" hold this
  // line.
  pi.on("agent_end", async (_event, context) => {
    if (subagentEvent(context)) return;
    stallWatch?.turnEnded();
  });

  /**
   * Names the session by its Legion identity (`src/session-title.ts`), so every Dispatch
   * write stamps it as `origin.session_title` and the Envoy listener lists it. Runs before the
   * session claims its Envoy role: that claim registers the session, and the registration carries
   * the title then rather than at the next heartbeat. The `dispatch` command reads the title from
   * the session's `title` file, which envoy.ts rewrites only before a shell command whose session
   * name changed, so it is written here too, and the pane's first command already carries it.
   */
  const titleSession = async (context: SessionContext): Promise<void> => {
    const title = legionSessionTitle(classifySession(process.env), process.env.LEGION_PROJECT);
    if (title === undefined) return;
    await applySessionTitle(pi, context, title);
    const sessionID = context.sessionManager.getSessionId();
    if (sessionID === "") return;
    writeSessionTitle(
      dispatchSessionDirectory(process.env, sessionID),
      context.sessionManager.getSessionName?.() ?? title
    );
  };

  // The recreated-workspace notice this process owes its first run: set at session_start below,
  // with the id its copies carry, and owed until that run starts (agent_start), whatever starts it.
  let recreatedNotice: WorkspaceRecreatedNotice | undefined;

  /**
   * The rest of `session_start` once the subagent check has answered, from the title through the
   * step that activates the `legion` tool: every step named and bounded, so that every failure
   * and every overrun of it reaches the one ending (`endHandler`, R1 and R3). A session the
   * classifier reads as no Legion session returns from here and leaves the process running, as
   * it always has.
   */
  const startLegionSession = async (
    context: SessionContext,
    bound: StepBound | undefined
  ): Promise<void> => {
    await boundedStep(bound, "titleSession", () => titleSession(context));
    // The session this pane's claim is on, for the reminder's timers and its record check, which
    // run outside every hook.
    paneContext = context;
    // A worker the daemon relaunched with --resume keeps its phase: its next turn may start from
    // an Envoy notice rather than a new assignment, and must find the phase still open. The
    // transcript is this host's store for that record; the runner is handed the record alone.
    await boundedStep(bound, "restorePhaseReminder", () =>
      phaseReminder.restore(restorePhaseStallEntry(context.sessionManager.getBranch?.() ?? []))
    );
    // The model fallback's own restore waits for the claim below: its save writes an entry into
    // the session's transcript, and a session no Legion claim is on is nobody's role to move or
    // to write into.
    const session = await boundedStep(bound, "classifySession", () => classifySession(process.env));
    if (session.kind === "not-legion") return;
    // Every Legion session (a root architect, a phase worker, the controller) claims through the
    // Envoy plugin's interface, so a process without it ends here, the way a refused boot
    // registration does: one log line, then the exit the daemon sees and relaunches from. That
    // ending is its own, taken in a hand-started session as much as in the daemon's agent, and
    // the cover around this handler adds nothing to it. A person's own session returned above
    // and never exits.
    const refusal = await boundedStep(bound, "envoyPluginRefusal", () => envoyPluginRefusal());
    if (refusal !== undefined) {
      logger.error(refusal);
      endProcess(1);
    }
    // Every Legion session watches its own tool calls; a subagent's instance returned above.
    await boundedStep(bound, "armStallWatch", () => armStallWatch(session));
    // The controller registers through the controller session and gets the `legion` tool's
    // controller operations (`read_state`, `set_status`), each minting its own controller grant.
    if (session.kind === "controller") {
      await boundedStep(bound, "controllerClaim", () =>
        controllerSession.handleSessionStart(context)
      );
      // The controller reports no activity — the daemon releases no pod of its — so it has no
      // managed interval of its own, and the stall watch takes one here: its session runs tool
      // calls that can stick like any other's.
      await boundedStep(bound, "armSessionWatches", () =>
        context.setInterval(stallSweep, SESSION_ACTIVITY_PERIOD_MS)
      );
    } else {
      // A resume whose role launcher found the workspace recreated since the session was last
      // written (src/workspace-recreated.ts) is told so in a message saved to the session ahead of
      // the next turn, whatever starts that turn (a task, an Envoy delivery), so a process lost in
      // that turn resumes with the notice already in its history. It goes before the role claim, so
      // no delivery on the role's topic can start a turn ahead of it, and a subagent, which returned
      // above, never is told.
      await boundedStep(bound, "workspaceRecreatedNotice", () => {
        recreatedNotice = workspaceRecreatedNotice(
          process.env,
          session.kind === "root-architect" ? session.tree : session.issue
        );
        if (recreatedNotice !== undefined) {
          pi.sendMessage(workspaceRecreatedMessage(recreatedNotice), {
            deliverAs: "steer",
            triggerTurn: false,
          });
        }
      });
      await boundedStep(bound, "claimBoot", () => claimSession.bootstrap(context));
      await boundedStep(bound, "armSessionWatches", () => {
        // The claim is registered. A phase worker relaunched into an open phase gets its reminder
        // from a timer, since nothing else would wake it. The guard drops that timer when any turn
        // started meanwhile, which is exactly the case to drop — the daemon re-sending an
        // interrupted task arrives as a prompt.
        if (phaseWorkerSession(context)) phaseReminder.armResumeReminder();
        /**
         * INFRA-927: the session measures its six capability rows on this interval, while it is
         * idle, up to three times, until every row is present — a check that lost the boot-time
         * race stayed reported open for the session's whole life, since the boot measured once and
         * never again. The architect measures first at the sixth tick as every other role does
         * (`CAPABILITY_FIRST_DUE_TICK` plus its index in `LEGION_ROLES`); its extra readies make
         * its waiting notices due at once, which is the same release a relaunched architect's ready
         * already makes.
         *
         * The measurement's input is snapshotted here, since the host's accessors answer only from
         * a handler and throw at factory time, and it is read before `registerLegionTool()` below,
         * so the rows see the tool surface `session_start` exposes.
         */
        const capabilityClaim = claimSession.capability(context.sessionManager.getSessionId());
        let capabilitySchedule: CapabilitySchedule | undefined;
        if (capabilityClaim !== undefined) {
          const capabilityInput = {
            cwd: context.cwd,
            sessionId: capabilityClaim.sessionID,
            issue: capabilityClaim.issue,
            promptAgents: capabilityClaim.promptAgents ?? [],
            activeTools: pi.getActiveTools(),
            agentsExposed: pi.agents !== undefined,
          };
          capabilitySchedule = createCapabilitySchedule({
            claim: capabilityClaim,
            measure: () =>
              measureCapabilities(capabilityHost(), capabilityInput, capabilityBudgets),
            send: (report, signal) =>
              roleDaemon().ready(
                {
                  claimToken: capabilityClaim.claimToken,
                  sessionId: capabilityClaim.sessionID,
                  secret: capabilityClaim.secret,
                  generation: capabilityClaim.generation,
                  capabilities: report,
                },
                { signal }
              ),
            sendTimeoutMs: capabilitySendTimeoutMs,
            // Live, as the activity report reads it: a context captured once would answer for the
            // session as it was at its start.
            isIdle: () => paneContext?.isIdle?.(),
            log: (message, fields) => logger.warn(message, fields),
          });
        }
        // Between turns, the host's own managed interval is the only sight of this session's
        // background work; it sends nothing unless what it reads has changed. Every claimed
        // session reports, an architect's as much as a phase worker's: the daemon releases a pod
        // only when each of its live roles has said it runs nothing. The stall watch's sweep rides
        // the same 5 seconds, which bounds how late a stuck call is found against a ladder whose
        // own steps are minutes apart.
        context.setInterval(() => {
          if (paneContext !== undefined) {
            void reportActivity(paneContext, { onlyWhenChanged: true });
          }
          // None of the three is awaited: a measurement that runs for up to 10 s, or a step of the
          // ladder that outlasts the tick, never delays the activity report the daemon reads to
          // decide whether the pod may be released.
          void capabilitySchedule?.tick();
          stallSweep();
        }, SESSION_ACTIVITY_PERIOD_MS);
      });
    }
    // The claim is registered, so this session is a Legion role's: a worker the daemon
    // relaunched into a session that fell back down the model chain comes back with the due time
    // of its return and how often each model's wait has doubled (src/model-fallback.ts).
    await boundedStep(bound, "restoreModelFallback", () => restoreModelFallback(context));
    await boundedStep(bound, "registerLegionTool", () => registerLegionTool());
    await boundedStep(bound, "activateLegionTool", () => activateLegionTool());
  };

  /**
   * The whole handler runs under one bound and one ending (R1, R3): a failure of any step of it,
   * and a step that outlives {@link LEGION_SESSION_START_BOUND_MS}, write one line naming the
   * step and end the process, in the agent the daemon launched for a role and in no other
   * process. The bound is below Oh My Pi's own {@link OMP_HANDLER_BOUND_MS}, so this handler
   * decides its own ending rather than being aborted into a session that runs on with no
   * `legion` tool.
   */
  pi.on("session_start", async (_event, context) => {
    const bound = handlerBound();
    // A `task`-spawned subagent session loads a fresh instance of this whole module: bail out
    // before classification, or the inherited LEGION_* environment would look like a fresh
    // root/worker boot and its failure would exit the parent process. See isSubagentSession.
    //
    // It is this handler's first statement and the one step that ends nothing: until it has
    // answered, this instance may be a subagent's, inside a process that holds its parent's
    // claim and may be running its turn. A failure of this step, and an overrun of it, are
    // written as one line here and this instance returns.
    let subagent: boolean;
    try {
      subagent = await boundedStep(bound, "checkSubagentSession", () =>
        checkSubagentSession(context)
      );
    } catch (error) {
      const { step, why } = stepFailure(error);
      logger.error(
        `[legion] session_start failed at ${step}: ${why}; this instance returns and the process runs on, since it may be a task subagent's`
      );
      return;
    }
    if (subagent) return;
    try {
      await startLegionSession(context, bound);
    } catch (error) {
      endHandler("session_start", bound, error);
    }
  });

  // A prompt (the daemon's task as an RPC `prompt`, or a person's direct message pi-envoy
  // delivers as a user turn) first recovers a failed last turn, and Oh My Pi recovers an empty
  // `length` stop by moving the branch back to that turn's parent, which takes the copy saved
  // after it off the branch. before_agent_start runs after that recovery, so it puts the notice
  // back into the run's messages when the branch no longer holds this process's copy, a notice an
  // earlier recreation saved not counting, and the run saves it on the branch. An Envoy card,
  // sent with triggerTurn, starts its turn with neither the recovery nor before_agent_start, so
  // its branch keeps the saved copy.
  let resentNoticeId: string | undefined;
  pi.on("before_agent_start", async (_event, context) => {
    if (recreatedNotice === undefined) return undefined;
    const branch = context.sessionManager.getBranch?.() ?? [];
    if (branchHoldsWorkspaceRecreatedNotice(branch, recreatedNotice.id)) return undefined;
    resentNoticeId = recreatedNotice.id;
    return { message: workspaceRecreatedMessage(recreatedNotice) };
  });
  pi.on("agent_start", async (_event, context) => {
    recreatedNotice = undefined;
    clearStartupTurn(context.sessionManager.getSessionId());
    // The operator moved on: a session change's re-claim outcome left in its widget comes down.
    controllerSession.handleAgentStart(context);
    // INFRA-1573: a turn of this session has started, whatever started it, so the claim boot
    // takes the session's Envoy name now rather than at its bound — the lane the listener kept
    // for the role is then handed over inside a turn that is already running, instead of taking
    // the session's first turn ahead of the daemon's task. A `task` subagent's event is not a
    // turn of this session (`subagentEvent`), and the controller's own claim is not this one's:
    // it sends its first turn and claims its role after it (`src/controller-session.ts`), and
    // this signal reaches a claim session that booted nothing there.
    if (!subagentEvent(context)) claimSession.firstTurnStarted();
  });
  pi.on("session_shutdown", async (_event, context) => {
    clearStartupTurn(context.sessionManager.getSessionId());
    // Whatever this session had open is not this watch's business any more.
    stallWatch?.sessionChanged();
  });
  // Every provider request of this session passes here, so this is where the session goes back
  // on the model it fell back from: the host checks its own restore only when a new prompt
  // arrives, which a long unattended tool loop never reaches (src/model-fallback.ts).
  //
  // The rest of the handler is the recreated-workspace notice: the recovery kept the saved copy
  // in the process's live context, so once it is sent again every later request of this process
  // would carry it twice: the first copy, where the history put it, is the one each request
  // keeps.
  pi.on("context", async (event, context) => {
    if (await modelFallbackSession(context)) await modelFallback.maybeRestore(context);
    if (resentNoticeId === undefined) return undefined;
    const messages = withoutRepeatedWorkspaceRecreatedNotice(event.messages, resentNoticeId);
    return messages === undefined ? undefined : { messages };
  });

  // The host walked this session's own fallback chain: the plugin owns the return, since the
  // host's restore never comes for a refusal it pinned (dispatch://INFRA-956). The event's own
  // reason says whether a content filter earned the walk, and a settled turn says so for a host
  // build whose event carries none (src/model-fallback.ts).
  pi.on("retry_fallback_applied", async (event, context) => {
    if (!(await modelFallbackSession(context))) return;
    modelFallback.armFromEvent(event, context);
  });

  // A turn that has settled: a content filter the host did not walk is walked here, a clean turn
  // puts the model's wait back to the base, and any other provider failure leaves it alone.
  pi.on("agent_end", async (event, context) => {
    if (!(await modelFallbackSession(context))) return;
    await modelFallback.turnSettled(event, context);
  });

  // Mirrors envoy.ts: only a switch reports why the session changed; a branch or a tree
  // navigation carries no reason, and every one of them can leave the pane on a new session id.
  // The controller re-claims whatever session the pane is left on, so that session takes the
  // controller's title first; a session that keeps its id keeps its title. A `task` subagent's
  // instance does neither, and the subagent check is asked once for both.
  const afterSessionChange = async (context: SessionContext): Promise<void> => {
    if (await checkSubagentSession(context)) return;
    // Whatever session the pane is left on, every reminder timer armed on the old one is stale,
    // and the new one is where the next is armed; the calls the old session had open are closed
    // with it, so a later stuck call starts the ladder again at its first step.
    paneContext = context;
    phaseReminder.sessionChanged();
    stallWatch?.sessionChanged();
    if (classifySession(process.env).kind === "controller") await titleSession(context);
    await controllerSession.reclaimAfterSessionChange(context);
    // The model fallback follows the pane the same way: the arming, the doubling levels and the
    // fallback this session may already be sitting on are the new branch's own, read as
    // session_start reads them. After the re-claim, which is what says whether the session the
    // pane was left on is a Legion role's at all: the controller's claim moves to it, and a
    // phase worker's stays on the session it was registered for.
    restoreModelFallback(context);
  };
  pi.on("session_switch", (_event, context) => afterSessionChange(context));
  pi.on("session_branch", (_event, context) => afterSessionChange(context));
  pi.on("session_tree", (_event, context) => afterSessionChange(context));

  // The daemon's assignment (a user message) opens the phase; an Envoy delivery re-arms a stall
  // whose reminders went unanswered or that a WAITING reply quieted. A person's direct message
  // that envoy.ts sent in as the user's own turn is a user message too (a `skill-prompt` custom
  // message for a `/skill:`), but it is an inbound event, as its Envoy card was: the record
  // envoy.ts keeps of the turns it sent in says which message that is, whichever of the two
  // extensions asks first. A turn that record misses falls through to `inboundKind` and counts as
  // an assignment; the phase-stall section of `packages/pi-legion/AGENTS.md` says which turns it
  // misses. The `legion` tool's successful `handoff_complete`, and a successful backward move,
  // close the phase (`onPhaseCompleted`, below). Every message, classified or not, says a turn is
  // running and leaves every armed reminder timer stale.
  pi.on("message_start", async (event, context) => {
    if (!phaseWorkerSession(context)) return;
    phaseReminder.turnStarted();
    const injected =
      matchInjectedUserTurn(context.sessionManager.getSessionId(), event.message) !== undefined;
    const kind = injected ? "inbound-event" : inboundKind(event.message);
    if (kind !== undefined) phaseReminder.apply({ kind });
  });

  // An assistant message that settled: one that ended aborted closes every call the stall watch
  // holds open, since the host ended the turn they belonged to, and that is where an aborted turn
  // is readable at all. One that ran at least one real tool call also says the worker is working,
  // so nothing it has been reminded of is unanswered — read from `message_end` rather than from
  // the `tool_call` handler above, which records a call's arguments and says nothing about
  // whether the turn it belongs to settled.
  //
  // **The line is a guard and a close, and each half has a case of its own**, measured by
  // cutting each alone: cutting the close reds "an assistant message that ended aborted closes
  // the calls of the turn it ended", and cutting the guard reds "a subagent's run cancelled on
  // its own closes none of the worker's calls". What no suite can say is that the two hold each
  // other. The guard's case is about what this close does **not** do for a subagent, so it
  // means something only while the close is here: take the close away with the case that holds
  // it — which is what removing a line read as dead looks like — and the guard's case goes
  // hollow rather than red, passing with the guard gone too. So the pair goes together or not
  // at all, and a green run after the first deletion is no licence for the second.
  pi.on("message_end", async (event, context) => {
    if (!subagentEvent(context)) stallWatch?.assistantMessage(event.message);
    if (!phaseWorkerSession(context)) return;
    if (assistantRanTool(event.message)) phaseReminder.apply({ kind: "tool-call" });
  });

  // The run is about to settle with nothing left for the host to continue: a phase still open
  // earns a reminder (src/phase-stall.ts). The first of a run comes back as the host's own
  // continuation, one more turn of this same session; every one after it is a timer, so the run
  // settles and the daemon sees the turn end (src/phase-reminder.ts).
  pi.on("session_stop", async (event, context) => {
    if (event.signal.aborted) return undefined;
    if (!phaseWorkerSession(context)) {
      // An architect has no phase of its own to settle, and its pod is released on the same rule
      // as a worker's, so its turn ends with a report of its own.
      await reportActivity(context, { onlyWhenChanged: false });
      return undefined;
    }
    const reminder = await phaseReminder.settle(assistantText(event.last_assistant_message));
    // After the settle, so the report carries the wake that settle armed: the daemon releases no
    // claim whose last report is older than its last turn, so this one goes whatever it says.
    await reportActivity(context, { onlyWhenChanged: false });
    return reminder === undefined ? undefined : { continue: true, additionalContext: reminder };
  });

  const onPhaseCompleted = (context: SessionContext): void => {
    if (phaseWorkerSession(context)) phaseReminder.apply({ kind: "phase-closed" });
  };

  // A successful `ready_for_update`: the worker has pushed what it wants kept and waits for the
  // process its claim runs in to be replaced. Its phase stays open, and the reminder must not
  // continue the turn it answered in — the work that continuation did would go with the pod, and
  // the daemon would see no turn end.
  const onUpdateAnswered = (context: SessionContext): void => {
    if (phaseWorkerSession(context)) phaseReminder.apply({ kind: "update-answered" });
  };

  const activateLegionTool = async (): Promise<void> => {
    const activeTools = pi.getActiveTools();
    if (activeTools.includes("legion")) return;
    await pi.setActiveTools([...activeTools, "legion"]);
  };

  // The session the tool answers for: a claim's (a root architect's, a sub-architect's or a phase
  // worker's, keyed by its role) or the claimed controller's; a `task` subagent holds neither and
  // is refused, as is a session whose boot has not registered yet.
  let legionToolRegistered = false;
  const registerLegionTool = (): void => {
    if (legionToolRegistered) return;
    legionToolRegistered = true;
    pi.registerTool(
      createLegionTool({
        pi,
        daemon: roleDaemon,
        session: (context) => {
          const sessionID = context.sessionManager.getSessionId();
          const active = claimSession.capability(sessionID);
          if (active !== undefined) {
            return { kind: active.role === "architect" ? "architect" : "phase-worker", ...active };
          }
          if (controllerSession.isClaimedSession(sessionID)) {
            return { kind: "controller", sessionID };
          }
          throw new Error("legion is available only to this session's registered claim");
        },
        controllerGrant: (sessionId) => controllerSession.mintGrant(sessionId),
        onPhaseCompleted,
        onUpdateAnswered,
        // The pane's own environment: its workspace and issue; PATH's jj, the one its shell runs;
        // and the Dispatch pi-envoy's session reads (Oh My Pi's rule, in the same process), which
        // the pane's `dispatch handoff-write` wrote the record to.
        handoffReport: (phase, role) =>
          paneHandoffReport(
            sessionConfigFile("omp", process.env),
            process.env,
            process.cwd(),
            currentSessionID()
          )(phase, role),
        branchHead: () => findPushedBranchHead({ env: process.env }),
        resolveDocument: async (issue, reference) => {
          // The file pi-envoy's session reads: Oh My Pi's rule, in the same process.
          const config = activeDispatchSettings(
            sessionDispatchSettings(sessionConfigFile("omp", process.env), process.env, {
              cwd: process.cwd(),
              session: currentSessionID,
            }).dispatch
          );
          if (config === null) {
            throw new Error(
              `register_gate cannot look up document "${reference}" on ${issue}: this pane has no Dispatch configured; pass the document's id`
            );
          }
          try {
            return await resolveIssueDocumentId(
              new DispatchClient(config.url, config.credential, fetch),
              issue,
              reference
            );
          } catch (error) {
            throw new Error(
              `register_gate could not look up document "${reference}" on ${issue} in Dispatch: ${messageFor(error)}`
            );
          }
        },
      })
    );
  };

  pi.registerCommand("legion-claim-controller", {
    description: "Claim the Legion controller role and register daemon authority for this session",
    // Inside the controller's own session (the `LEGION_CONTROLLER` marker) this is the manual
    // override for a lost claim. From a hand-started session it is an interactive takeover: the
    // role and recorded session id move to this session (see `controllerSession.claim`), which
    // then gets the `legion` tool's controller operations as a launched controller does.
    //
    // It is the one path besides `session_start` that registers the `legion` tool, so it takes
    // the same cover (R1): in the agent the daemon launched for a role — the controller it
    // launched itself among them — every failure and every overrun of this handler writes one
    // line naming the step and ends the process. In any other process nothing is ended: the
    // operator started that session and reads it, so a missing Envoy plugin and every other
    // failure are answered as the command's error and nothing is claimed. `legion controller
    // start` deletes both boot-token names from the controller it launches for an operator,
    // which is what leaves that session on this second footing.
    handler: async (_args, context) => {
      const bound = handlerBound();
      try {
        await boundedStep(bound, "envoyPluginRefusal", () => {
          const refusal = envoyPluginRefusal();
          if (refusal !== undefined) throw new Error(refusal);
        });
        await boundedStep(bound, "controllerClaim", () => controllerSession.claim(context));
        // The claim is registered, so this session is the controller's: the arming and the
        // doubling levels its branch records are read back here, as session_start reads a worker's.
        await boundedStep(bound, "restoreModelFallback", () => restoreModelFallback(context));
        await boundedStep(bound, "registerLegionTool", () => registerLegionTool());
        await boundedStep(bound, "activateLegionTool", () => activateLegionTool());
      } catch (error) {
        endHandler("/legion-claim-controller", bound, error);
      }
    },
  });

  pi.registerCommand("legion-controller-start", {
    description:
      "Claim the Legion controller and send its start message as the session's first turn; `legion controller start` passes this command on Oh My Pi's command line",
    // Why a command and not a claim from session_start: `controllerSession.startCommand`.
    handler: async (_args, context) => {
      await controllerSession.startCommand(context);
      // The operator-launched controller registers its claim nowhere else, so this is where a
      // controller resumed on a fallback gets its state back. The command keeps its own refusals
      // (`controllerSession.startCommand`), and a claim that did not register leaves this
      // session holding none, which the restore answers by doing nothing.
      restoreModelFallback(context);
    },
  });
}
