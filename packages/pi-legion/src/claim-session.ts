import { daemonContract } from "@legion/contracts/daemon-contract";
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
import { claimRefusedForStop } from "@legion/pi-shared/role-claim-stopping";
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
import { exitAfterHostStop, type HostStopExit } from "./host-stop";

/** The daemon API contract this build speaks, which its registration states as `pluginContract`
 * (`daemonContract`, read once as the module loads). */
const PLUGIN_CONTRACT = daemonContract(pkg, import.meta.dir);

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
  /** The session's stop has begun (`legion.ts` calls it at `session_shutdown`), R24 of
   * dispatch://INFRA-3589: the boot's signal aborts, so its role claim is not made, a failed
   * `claims/ready` is neither said nor sent again, and a claim that fails from then on says
   * nothing and exits nothing. A claim Envoy refuses because the session's stop has begun
   * (`claimRefusedForStop`) is that same stop: in RPC mode a SIGTERM stops Envoy's session with
   * no `session_shutdown`. */
  readonly stop: () => void;
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
 * exit the process, except once the session's stop has begun ({@link ClaimSession.stop}): the
 * stop ends the process itself, and an exit in its middle would cut its role release and its
 * DELETE short. The host's stop can begin before this session hears of its own: in RPC mode a
 * SIGTERM runs pi-envoy's stop and emits no `session_shutdown`. A refusal or a failure that comes
 * then, such as a ready the daemon refuses once its registration deadline retired this process,
 * exits once the host's stop has ended, and at most at its bound (`exitAfterHostStop`), with the
 * same status.
 */
export function createClaimSession(
  deps: HostStopExit & {
    readonly daemon: () => LegionDaemonClient;
    readonly persistedTranscript: (
      context: SessionContext
    ) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;
    /** An accepted take's text to the host (`TaskTakeDeps.run`). */
    readonly runTask: TaskTakeDeps["run"];
    readonly reportDrop: (
      sessionID: string,
      ...drop: Parameters<NonNullable<TaskTakeDeps["reportDrop"]>>
    ) => Promise<void>;
  }
): ClaimSession {
  let capability: ClaimCapability | undefined;
  let booting: Promise<void> | undefined;
  let taking: Taking | undefined;
  // Ends the take's hold on its mark, kept until the read after the ready has answered
  // (`afterReadyRead`).
  let readAnswered: (() => void) | undefined;
  // Aborts as the session's stop begins: the boot's signal (`LegionClaimHost.signal`).
  const stopped = new AbortController();

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
        pluginContract: PLUGIN_CONTRACT,
      },
      afterRegister: async (claim) => {
        const caller = claimCaller(claim, sessionID);
        taking = {
          ...takeClaimTask({
            daemon,
            caller,
            run: deps.runTask,
            reportDrop: (...drop) => deps.reportDrop(sessionID, ...drop),
            log: logger,
          }),
          caller,
        };
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
      // and no phase worker is woken by an architect's notice. `replaces`, the session the
      // registration says this one replaces, is the claim's `previous_session_id`, so its unread
      // messages come here.
      claimRole: async (role, replaces) => {
        try {
          await claimEnvoyRole(sessionID, role, context, replaces);
        } catch (error) {
          // Envoy refused the claim because the session's stop has begun: the stop is this
          // boot's too, so the failure ends the boot quietly (`bootLegionClaim`).
          if (claimRefusedForStop(error)) stopped.abort();
          throw error;
        }
      },
      taskValue: (reading) => taking?.take.read(reading),
      afterReadyRead: () => readAnswered?.(),
      onRoleRegained: onEnvoyRoleRegained,
      log: (line) => console.error(`[legion] ${line}`),
      signal: stopped.signal,
      stopping: "exiting so the daemon launches it again",
      // A boot the session's stop overtook exits nothing: the stop ends the process itself. A boot
      // that fails while the host's stop runs exits once that stop has ended.
      stop: () => {
        if (!stopped.signal.aborted) void exitAfterHostStop(deps, 1);
      },
    });
    if (claimBoot === undefined) return;
    const take = taking?.take;
    setImmediate(() => take?.ready());
  };

  const bootstrap = async (context: SessionContext): Promise<void> => {
    const sessionID = context.sessionManager.getSessionId();
    if (capability !== undefined && capability.sessionID !== sessionID) return;
    if (booting !== undefined) return booting;

    booting = requiredSecret(process.env, "LEGION_BOOT_TOKEN").then((bootToken) =>
      boot(context, sessionID, bootToken, deps.daemon())
    );
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
    stop: () => stopped.abort(),
  };
}
