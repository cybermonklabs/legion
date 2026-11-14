import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import {
  exportJjSessionAttribution,
  paneAttributionOptions,
  sessionIdFromTranscript,
} from "@legion/pi-shared/jj-attribution";
import { bootLegionClaim, type LegionClaimBoot } from "@legion/pi-shared/legion-claim-core";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { SessionContext } from "@legion/pi-shared/pi-types";
import { claimEnvoyRole, onEnvoyRoleRegained } from "@legion/pi-shared/role-claim-bridge";
import { recordBootstrappedSession } from "@legion/pi-shared/subagent-session";
import { createTaskTake, type TaskTake } from "@legion/pi-shared/task-take";
import {
  clearTaskTurnPending,
  markTaskTurnPending,
  takeTaskSignals,
} from "@legion/pi-shared/task-values";
import { logger } from "@oh-my-pi/pi-utils";
import pkg from "../package.json";
import { requiredEnvironment, requiredSecret } from "./classify";

export interface ClaimSession {
  /** Boots the claim behind a root architect's or a phase worker's pane (`legion.ts` calls it for
   * every Legion session but the controller). A second call while the first runs waits on it; a
   * call once the claim is registered, from its session or another, boots nothing. */
  readonly bootstrap: (context: SessionContext) => Promise<void>;
  /** The claim `sessionID` registered, or undefined: before its registration, and for any other
   * session. */
  readonly capability: (sessionID: string) => ClaimCapability | undefined;
  /** A run of this pane's session started (`legion.ts` calls it from `agent_start`, never for a
   * `task` subagent's event): the claim's take no longer finds the host idle, and the first run
   * after an accepted take is that task's turn (`@legion/pi-shared/task-take`). It does nothing
   * before the claim is registered. */
  readonly runStarted: () => void;
  /** That run ended (`agent_end`): the number of the task whose turn it was, which the turn's
   * outcome report names, or undefined for a run of no task or a session with no claim. */
  readonly runEnded: () => number | undefined;
}

/**
 * What this pane's jj attribution overlay is written from
 * (`exportJjSessionAttribution`, `@legion/pi-shared/jj-attribution`): the Oh My Pi session id read
 * off the transcript (undefined for a subagent's, which owns no claim), the state directory the
 * daemon names, and the pane's own key and workspace (`paneAttributionOptions`, which holds the
 * rule both harnesses read those two variables by).
 */
export function attributionInputs(
  sessionFile: string,
  env: NodeJS.ProcessEnv
): {
  readonly sessionId: string | undefined;
  readonly stateDir: string;
  readonly issueKey?: string;
  readonly workspace?: string;
} {
  return {
    sessionId: sessionIdFromTranscript(sessionFile),
    stateDir: requiredEnvironment(env, "LEGION_STATE_DIR"),
    ...paneAttributionOptions(env),
  };
}

/**
 * Owns a root architect's or a phase worker's claim: the boot, through the claim boot every
 * harness shares (`bootLegionClaim`, `@legion/pi-shared/legion-claim-core`), the capability it
 * registered, and the take of the claim's task (`createTaskTake`, `@legion/pi-shared/task-take`).
 * A Legion pane boots as its own Oh My Pi process and holds exactly one claim for its whole
 * lifetime, so both are plain closure state, set before `claims/ready`. The ready carries no
 * capability report: the session's capability rows are measured later, on its own interval.
 *
 * The daemon pushes no task. The take holds the claim's value from the registration's answer on,
 * which is also when it sets the task-turn mark, before the role is claimed; it takes each value
 * the Envoy entry hands over from a task frame the listener relayed, and reads the value again
 * from the daemon whenever the session's relay begins a new epoch and whenever the heartbeat
 * regains the role (`takeTaskSignals`, `readTask`). The Envoy name is taken right after
 * `claims/ready` has answered, so the session is addressable from its ready on, and the lane the
 * listener kept for the role arrives while the mark holds its cards back.
 *
 * The take asks only from a timer that fires once `session_start` has returned: Oh My Pi holds a
 * send made from `session_start` until its startup is over, and runs a send made while its
 * startup drains its sends inside that drain, while RPC mode reads no command at all, so a task's
 * turn started there would keep the daemon's shim from reaching the session for the whole turn.
 * The timer is a macrotask, and the drain's end, up to the return of the host's extension
 * startup, runs in microtasks (`packages/pi-envoy/src/card-wake.ts`, "The startup phase"). An
 * accepted take's text goes to `runTask`.
 *
 * The transcript is recorded as the bootstrapped session before the registration; the jj
 * attribution (`exportJjSessionAttribution`) runs once it answered, before the capability is set.
 * A 4xx registration refusal, and any failure after a registration, exit the process.
 */
export function createClaimSession(deps: {
  readonly daemon: () => LegionDaemonClient;
  readonly persistedTranscript: (
    context: SessionContext
  ) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;
  /** Sends an accepted take's text as the turn's user message. */
  readonly runTask: (text: string) => void;
  readonly exitProcess: (code: number) => never;
}): ClaimSession {
  let capability: ClaimCapability | undefined;
  let booting: Promise<void> | undefined;
  let take: TaskTake | undefined;

  const boot = async (
    context: SessionContext,
    sessionID: string,
    bootToken: string,
    daemon: LegionDaemonClient
  ): Promise<void> => {
    const { sessionFile, agentId } = await deps.persistedTranscript(context);
    recordBootstrappedSession(sessionFile);
    // Set before the registration: a value relayed before the take exists is one the
    // registration's answer carries anyway.
    let claimBoot: LegionClaimBoot | undefined;
    takeTaskSignals((signal) => {
      if (signal.session !== sessionID) return;
      if (signal.kind === "value") take?.relayed(signal.value);
      else void claimBoot?.readTask();
    });
    claimBoot = await bootLegionClaim({
      daemon,
      registration: {
        bootToken,
        sessionId: sessionID,
        ompSessionFile: sessionFile,
        agentId,
        pluginContract: pkg.legion.daemonApiVersion,
      },
      afterRegister: async (claim) => {
        take = createTaskTake({
          daemon,
          caller: {
            claimToken: claim.claimToken,
            sessionId: sessionID,
            secret: claim.secret,
            generation: claim.generation,
          },
          run: deps.runTask,
          setPending: (pending) => {
            if (pending) markTaskTurnPending(sessionID);
            else clearTaskTurnPending(sessionID);
          },
          log: logger,
        });
        take.read(claim);
        const { sessionId, stateDir, ...pane } = attributionInputs(sessionFile, process.env);
        if (sessionId) await exportJjSessionAttribution(sessionId, stateDir, pane);
      },
      setCapability: (registered) => {
        capability = registered;
      },
      // The claim's role topic is where the daemon sends every notice for an architect (the
      // notice executor, packages/daemon/internal/daemon/outbox.go), and where the listener
      // relays the claim's task value: no issue topic carries one, so no claim subscribes to one.
      claimRole: (role) => claimEnvoyRole(sessionID, role, context),
      taskValue: (reading) => take?.read(reading),
      onRoleRegained: onEnvoyRoleRegained,
      log: (line) => console.error(`[legion] ${line}`),
      stopping: "exiting so the daemon launches it again",
      stop: () => deps.exitProcess(1),
    });
    if (claimBoot === undefined) return;
    const taking = take;
    setImmediate(() => taking?.ready());
  };

  const bootstrap = async (context: SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    if (capability !== undefined && capability.sessionID !== sessionID) return;
    if (booting !== undefined) return booting;

    const bootToken = requiredSecret(process.env, "LEGION_BOOT_TOKEN");
    booting = boot(context, sessionID, bootToken, deps.daemon());
    try {
      await booting;
    } catch (error) {
      booting = undefined;
      throw error;
    }
  };

  return {
    bootstrap,
    capability: (sessionID) => (capability?.sessionID === sessionID ? capability : undefined),
    runStarted: () => take?.runStarted(),
    runEnded: () => take?.runEnded(),
  };
}
