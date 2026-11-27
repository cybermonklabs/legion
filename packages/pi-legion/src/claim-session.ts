import { daemonContract } from "@legion/contracts/daemon-contract";
import { claimFirstTurnBoundMs } from "@legion/pi-shared/claim-first-turn-bound";
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
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
  /** A turn of this pane's session has started, whatever started it: the daemon's task, an Envoy
   * card, the phase reminder (`legion.ts` calls it from `agent_start`, never for a `task`
   * subagent's event). It is the boot's first-turn signal: the deferred role claim runs now
   * instead of waiting out its bound. Calling it costs nothing once the name is taken, before the
   * boot has registered its claim step (the start is remembered and the step runs the moment it
   * registers), or in a session that boots no claim of its own, such as the controller's, which
   * claims its role for itself (`src/controller-session.ts`). */
  readonly firstTurnStarted: () => void;
  /** The session's stop has begun (`legion.ts` calls it at `session_shutdown`), R24 of
   * dispatch://INFRA-3589: the boot's signal aborts, so its deferred role claim is not made, a
   * failed `claims/ready` is neither said nor sent again, and a claim that fails from then on
   * says nothing and exits nothing. A claim Envoy refuses because the session's stop has begun
   * (`claimRefusedForStop`) is that same stop: in RPC mode a SIGTERM stops Envoy's session with
   * no `session_shutdown`. */
  readonly stop: () => void;
}

/**
 * What this pane's jj attribution overlay is written from
 * (`exportJjSessionAttribution`, `@legion/pi-shared/jj-attribution`): the Oh My Pi session id read
 * off the transcript (undefined for a subagent's, which owns no claim), the state directory the
 * daemon names, and the pane's own key, the repository of its workspace and its role
 * (`paneAttributionOptions`, which holds the rule both harnesses read those three variables by).
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
 * harness shares (`bootLegionClaim`, `@legion/pi-shared/legion-claim-core`), and the capability it
 * registered. A Legion pane boots as its own Oh My Pi process and holds exactly one claim for its
 * whole lifetime, so the capability is plain closure state, set before `claims/ready`: the
 * daemon's task can arrive the moment ready answers, and the phase-stall hook reads the capability
 * when it does. The ready carries no capability report: the session's capability rows are
 * measured later, on its own interval.
 *
 * The Envoy name is taken last, after `claims/ready` has answered and at the session's first
 * turn (INFRA-1573): the listener keeps a message published to a role no session holds and hands
 * the whole kept lane over the moment the role is claimed, so a name taken before the daemon's
 * task lets that lane take the session's first turn while the task waits for it to end, and a
 * name taken as a turn starts puts the lane inside a turn that is already running. The signal is
 * {@link ClaimSession.firstTurnStarted}, which `legion.ts` calls from `agent_start`, and the
 * bound the boot falls back to is this session's own `claimFirstTurnBoundMs`, so a role brought
 * back with no task of its own still takes its name and still hears what waited for it. Only the
 * claim is deferred: the boot itself is still awaited inside `session_start`, where no turn of
 * this session can begin.
 *
 * The transcript is recorded as the bootstrapped session before the registration; the jj
 * attribution (`exportJjSessionAttribution`) runs once it answered, before the capability is set.
 * A 4xx registration refusal, and any failure after a registration, exit the process, except
 * once the session's stop has begun ({@link ClaimSession.stop}): the stop ends the process
 * itself, and an exit in its middle would cut its role release and its DELETE short. The host's
 * stop can begin before this session hears of its own: in RPC mode a SIGTERM runs pi-envoy's stop
 * and emits no `session_shutdown`. A refusal or a failure that comes then, such as a ready the
 * daemon refuses once its registration deadline retired this process, exits once the host's stop
 * has ended, and at most at its bound (`exitAfterHostStop`), with the same status.
 */
export function createClaimSession(
  deps: HostStopExit & {
    readonly daemon: () => LegionDaemonClient;
    readonly persistedTranscript: (
      context: SessionContext
    ) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;
  }
): ClaimSession {
  let capability: ClaimCapability | undefined;
  let booting: Promise<void> | undefined;
  // The boot's first-turn signal, held from the moment the boot registers it, and whether a turn
  // of this session has started. Calling it reports a turn and claims nothing: the claim step the
  // boot runs after `claims/ready` is what takes the name. The two arrive in either order and both
  // orders take the name exactly once: a turn that starts before the boot registers the signal is
  // remembered here and reported the moment it registers, and a second turn reports a turn already
  // reported, which does nothing (`listenForFirstTurn`, `@legion/pi-shared/legion-claim-core`).
  let reportFirstTurn: (() => void) | undefined;
  let turnStarted = false;
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
    await bootLegionClaim({
      daemon,
      registration: {
        bootToken,
        sessionId: sessionID,
        ompSessionFile: sessionFile,
        agentId,
        pluginContract: PLUGIN_CONTRACT,
      },
      afterRegister: async () => {
        const { sessionId, stateDir, ...pane } = attributionInputs(sessionFile, process.env);
        if (sessionId) await exportJjSessionAttribution(sessionId, stateDir, pane);
      },
      setCapability: (registered) => {
        capability = registered;
      },
      // The claim's role topic is where the daemon sends every notice for an architect (the
      // notice executor, packages/daemon/internal/daemon/outbox.go): no issue topic carries one,
      // so no claim subscribes to one. `replaces`, the session the registration says this one
      // replaces, is the claim's `previous_session_id`, so its unread messages come here.
      claimRole: async (role, replaces) => {
        try {
          await claimEnvoyRole(sessionID, role, context, replaces);
        } catch (error) {
          // Envoy refused the claim because the session's stop has begun: the stop is this
          // boot's too, so the failure ends the claim step quietly (`claimRoleAtFirstTurn`).
          if (claimRefusedForStop(error)) stopped.abort();
          throw error;
        }
      },
      onFirstTurn: (started) => {
        reportFirstTurn = started;
        if (turnStarted) started();
      },
      // The pane's own bound, read from its environment as both watches of this entry read
      // theirs (`stallLimits` in `extensions/legion.ts`).
      firstTurnBoundMs: claimFirstTurnBoundMs(process.env, logger),
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
    firstTurnStarted: () => {
      turnStarted = true;
      reportFirstTurn?.();
    },
    stop: () => stopped.abort(),
  };
}
