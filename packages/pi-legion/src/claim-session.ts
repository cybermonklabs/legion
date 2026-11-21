import {
  type ClaimCaller,
  claimCaller,
  type LegionDaemonClient,
} from "@legion/pi-shared/daemon-client";
import {
  exportJjSessionAttribution,
  type PaneAttribution,
  paneAttributionOptions,
  sessionIdFromTranscript,
} from "@legion/pi-shared/jj-attribution";
import { bootLegionClaim } from "@legion/pi-shared/legion-claim-core";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { SessionContext } from "@legion/pi-shared/pi-types";
import { claimEnvoyRole, onEnvoyRoleRegained } from "@legion/pi-shared/role-claim-bridge";
import { recordBootstrappedSession } from "@legion/pi-shared/subagent-session";
import {
  type ClaimTaskTake,
  type TaskTake,
  type TaskTakeDeps,
  takeClaimTask,
} from "@legion/pi-shared/task-take";
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
  /** The take of this pane's claim (`@legion/pi-shared/task-take`), whose hooks `legion.ts` calls
   * for a run of the claim's session (`agent_start` and `agent_end`, never a `task` subagent's
   * event) and for the daemon's refusal of that run's report (`src/turn-outcome.ts`): the first
   * run after an accepted take's text was handed to the idle host is that task's turn, and its end
   * the report naming its number.
   * Undefined for any other session than the one the claim registered, which a switch, a branch
   * or a tree navigation can leave the pane on (`afterSessionChange` in `legion.ts`): a run of
   * that session neither starts nor ends the take's turn, as its end reports nothing
   * (`capability`). Undefined before the claim is registered. */
  readonly take: (sessionID: string) => TaskTake | undefined;
}

/** The take of this pane's claim (`takeClaimTask`), and the caller it takes for: the claim's
 * registration, made for the session the boot ran in. */
interface Taking extends ClaimTaskTake {
  readonly caller: ClaimCaller;
}

/**
 * What this pane's jj attribution overlay is written from
 * (`exportJjSessionAttribution`, `@legion/pi-shared/jj-attribution`): the Oh My Pi session id read
 * off the transcript (undefined for a subagent's, which owns no claim), the state directory the
 * daemon names, and the pane's own key, workspace and role (`paneAttributionOptions`, which holds
 * the rule both harnesses read those three variables by).
 */
export function attributionInputs(
  sessionFile: string,
  env: NodeJS.ProcessEnv
): PaneAttribution & {
  readonly sessionId: string | undefined;
  readonly stateDir: string;
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
 * registered, and the take of the claim's task (`takeClaimTask`, `@legion/pi-shared/task-take`).
 * A Legion pane boots as its own Oh My Pi process and holds exactly one claim for its whole
 * lifetime, so both are plain closure state, set before `claims/ready`. The ready carries no
 * capability report: the session's capability rows are measured later, on its own interval.
 *
 * The take holds the claim's value from the registration's answer on, which is also when it sets
 * the task-turn mark, and takes the value the boot reads again once the ready has answered, which
 * holds any value the ready decision wrote, both before the role is claimed; it takes each value
 * the Envoy entry hands over from a task frame the listener relayed, and reads the value again
 * from the daemon whenever the session's relay begins a new epoch and whenever the heartbeat
 * regains the role. From the registration until the read after the ready has answered, the take
 * holds the mark whatever value it holds (`TaskTake.awaitRead`): the session's agent subject is
 * open from its start, and a card addressed to it in that window, which the ready decision's
 * value may not have reached yet, waits like every other. The Envoy name is taken right after
 * that read, so the session is addressable from its ready on, and the lane the listener kept for
 * the role arrives while the mark holds its cards back where a task is owed.
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
 * A 4xx registration refusal, and any failure after a registration but the read after the ready,
 * exit the process.
 */
export function createClaimSession(deps: {
  readonly daemon: () => LegionDaemonClient;
  readonly persistedTranscript: (
    context: SessionContext
  ) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;
  /** An accepted take's text to the host (`TaskTakeDeps.run`). */
  readonly runTask: TaskTakeDeps["run"];
  readonly exitProcess: (code: number) => never;
}): ClaimSession {
  let capability: ClaimCapability | undefined;
  let booting: Promise<void> | undefined;
  let taking: Taking | undefined;
  // Ends the take's hold on its mark, kept until the read after the ready has answered
  // (`afterReadyRead`).
  let readAnswered: (() => void) | undefined;

  const boot = async (
    context: SessionContext,
    sessionID: string,
    bootToken: string,
    daemon: LegionDaemonClient
  ): Promise<void> => {
    const { sessionFile, agentId } = await deps.persistedTranscript(context);
    recordBootstrappedSession(sessionFile);
    const claimBoot = await bootLegionClaim({
      daemon,
      registration: {
        bootToken,
        sessionId: sessionID,
        ompSessionFile: sessionFile,
        agentId,
        pluginContract: pkg.legion.daemonApiVersion,
      },
      afterRegister: async (claim) => {
        const caller = claimCaller(claim, sessionID);
        taking = { ...takeClaimTask({ daemon, caller, run: deps.runTask, log: logger }), caller };
        taking.take.read(claim);
        readAnswered = taking.take.awaitRead();
        const { sessionId, stateDir, ...pane } = attributionInputs(sessionFile, process.env);
        if (sessionId) await exportJjSessionAttribution(sessionId, stateDir, pane);
      },
      setCapability: (registered) => {
        capability = registered;
      },
      // The claim's role topic is where the daemon sends every notice for an architect (the
      // notice executor, packages/daemon/internal/daemon/outbox.go), and where the listener
      // relays the claim's task value: no issue topic carries one, so no claim subscribes to one,
      // and no phase worker is woken by an architect's notice.
      claimRole: (role) => claimEnvoyRole(sessionID, role, context),
      taskValue: (reading) => taking?.take.read(reading),
      afterReadyRead: () => readAnswered?.(),
      onRoleRegained: onEnvoyRoleRegained,
      log: (line) => console.error(`[legion] ${line}`),
      stopping: "exiting so the daemon launches it again",
      stop: () => deps.exitProcess(1),
    });
    if (claimBoot === undefined) return;
    const take = taking?.take;
    setImmediate(() => take?.ready());
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
    take: (sessionID) => (taking?.caller.sessionId === sessionID ? taking.take : undefined),
  };
}
