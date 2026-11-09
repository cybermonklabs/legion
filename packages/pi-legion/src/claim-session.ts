import { claimFirstTurnBoundMs } from "@legion/pi-shared/claim-first-turn-bound";
import type { LegionDaemonClient } from "@legion/pi-shared/daemon-client";
import {
  exportJjSessionAttribution,
  paneAttributionOptions,
  sessionIdFromTranscript,
} from "@legion/pi-shared/jj-attribution";
import { bootLegionClaim } from "@legion/pi-shared/legion-claim-core";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import type { SessionContext } from "@legion/pi-shared/pi-types";
import { claimEnvoyRole, onEnvoyRoleRegained } from "@legion/pi-shared/role-claim-bridge";
import { recordBootstrappedSession } from "@legion/pi-shared/subagent-session";
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
  /** A turn of this pane's session has started, whatever started it: the daemon's task, an Envoy
   * card, the phase reminder (`legion.ts` calls it from `agent_start`, never for a `task`
   * subagent's event). It is the boot's first-turn signal: the deferred role claim runs now
   * instead of waiting out its bound. Calling it costs nothing once the name is taken, before the
   * boot has registered its claim step (the start is remembered and the step runs the moment it
   * registers), or in a session that boots no claim of its own, such as the controller's, which
   * claims its role for itself (`src/controller-session.ts`). */
  readonly firstTurnStarted: () => void;
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
 * A 4xx registration refusal, and any failure after a registration, exit the process.
 */
export function createClaimSession(deps: {
  readonly daemon: () => LegionDaemonClient;
  readonly persistedTranscript: (
    context: SessionContext
  ) => Promise<{ readonly sessionFile: string; readonly agentId: string }>;
  readonly exitProcess: (code: number) => never;
}): ClaimSession {
  let capability: ClaimCapability | undefined;
  let booting: Promise<void> | undefined;
  // The boot's deferred claim step, held from the moment it registers, and whether a turn of this
  // session has started. The two arrive in either order and both orders claim exactly once: a
  // turn that starts before the boot registers its step is remembered here and claims the moment
  // it registers, and a second turn calls a step that has already run, which does nothing
  // (`claimRoleAtFirstTurn`, `@legion/pi-shared/legion-claim-core`).
  let claimAtFirstTurn: (() => void) | undefined;
  let turnStarted = false;

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
        pluginContract: pkg.legion.daemonApiVersion,
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
      // so no claim subscribes to one.
      claimRole: (role) => claimEnvoyRole(sessionID, role, context),
      onFirstTurn: (claim) => {
        claimAtFirstTurn = claim;
        if (turnStarted) claim();
      },
      // The pane's own bound, read from its environment as both watches of this entry read
      // theirs (`stallLimits` in `extensions/legion.ts`).
      firstTurnBoundMs: claimFirstTurnBoundMs(process.env, logger),
      onRoleRegained: onEnvoyRoleRegained,
      log: (line) => console.error(`[legion] ${line}`),
      stopping: "exiting so the daemon launches it again",
      stop: () => deps.exitProcess(1),
    });
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
    firstTurnStarted: () => {
      turnStarted = true;
      claimAtFirstTurn?.();
    },
  };
}
