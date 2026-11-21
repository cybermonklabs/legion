import { messageFor } from "@legion/envoy-client/errors";
import {
  type ClaimCaller,
  claimCaller,
  type LegionDaemonClient,
} from "@legion/pi-shared/daemon-client";
import {
  createHoldsRead,
  type HeldTake,
  HOLDS_READ_BOUND_MS,
  type HoldsRead,
} from "@legion/pi-shared/holds-read";
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
  /** A run of `sessionID` started (`going` true, from `agent_start`) or ended (false, from
   * `agent_end`), never a `task` subagent's: the bounds of the run whose tool results read the
   * takes it holds (`heldNotes`). At each bound the holds read drops what earlier reads named and
   * a read still unanswered, since each take a read names is accepted at the end of the run it
   * was named in, even where that run's end never came. Nothing for any other session than the
   * one the claim registered, or before the claim is registered. */
  readonly runBoundary: (sessionID: string, going: boolean) => void;
  /** The number of the claim's own task to note at this tool result (dispatch://INFRA-4130 R1,
   * `taskWaitingNote`): the take's `noteFor`, which gives each number once and records it, so
   * only the host's call at a tool result of the session's own agent may make it. Undefined
   * where the session holds no take. */
  readonly taskNote: () => number | undefined;
  /** Whether the claim's take is pending (R6, `TaskTake.pending`), so that the phase reminder
   * starts no turn ahead of its task; false where the session holds no take. */
  readonly taskPending: () => boolean;
  /** Holds `callback`, the stall abort's restart message, for the claim's pending take: where the
   * take is pending, it keeps the callback, which is called once the take settles with no run
   * starting first, and answers the number of the task the take waits for
   * (`TaskTake.deferUntilSettled` says when, and which); undefined, keeping nothing, where the
   * take is not pending or the session holds no take. */
  readonly deferUntilTakeSettles: (callback: () => void) => number | undefined;
  /**
   * The host's call at each tool result of the session's own agent (R2): the takes of the
   * issue's other claims that the daemon holds behind this claim's turn, as the latest holds
   * read of this run named them, less those this process noted before (once per role and task).
   *
   * The call also starts that read while a run is going, at the first tool result or at the first
   * one a minute or more after the last read started: the claim's holds read
   * (`createHoldsRead`, `@legion/pi-shared/holds-read`), a `claims/task` with the claim's caller,
   * of which only `holds` is kept. The take is handed none of it, since its inputs are the
   * registration, the relay and its own reads. The tool result does not wait for the read, so its
   * answer is noted at the next tool result after it. A read bounded out (`HOLDS_READ_BOUND_MS`)
   * or failed notes nothing, an answer that comes after the run's end is dropped, and no read
   * starts while no run is going (`runBoundary`). Empty before the claim is registered.
   */
  readonly heldNotes: () => readonly HeldTake[];
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
 * Both notes of dispatch://INFRA-4130 are read here and written by the host at a tool result:
 * the take's own (`taskNote`), and the takes of the issue's other claims that the daemon holds
 * behind this claim's turn (`heldNotes`), which this session reads from the `holds` of its own
 * `claims/task` while a run of it is going. Every claim session reads them, a phase worker's and
 * an architect's alike, since every claim of an issue in a turn holds that issue's phase takes;
 * the controller's claim is not this session's and has no issue.
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
  /** The clock the holds reads are a minute apart by (`heldNotes`). */
  readonly now: () => number;
}): ClaimSession {
  let capability: ClaimCapability | undefined;
  let booting: Promise<void> | undefined;
  let taking: Taking | undefined;
  // Ends the take's hold on its mark, kept until the read after the ready has answered
  // (`afterReadyRead`).
  let readAnswered: (() => void) | undefined;
  // The holds read (`heldNotes`), made at the registration with the claim's caller, and whether a
  // run of the session is going, which alone reads it.
  let holdsRead: HoldsRead | undefined;
  let running = false;

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
        holdsRead = createHoldsRead({
          daemon,
          caller: () => caller,
          now: deps.now,
          failed: (error) => {
            logger.debug(
              `legion: a claims/task read of the takes held behind this turn failed or had no answer within ${HOLDS_READ_BOUND_MS} ms; it notes nothing`,
              { error: messageFor(error) }
            );
          },
        });
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
    runBoundary: (sessionID, going) => {
      if (taking?.caller.sessionId !== sessionID) return;
      running = going;
      holdsRead?.turnEnded();
    },
    taskNote: () => taking?.take.noteFor(),
    taskPending: () => taking?.take.pending() ?? false,
    deferUntilTakeSettles: (callback) => taking?.take.deferUntilSettled(callback),
    heldNotes: () => (running ? (holdsRead?.holds() ?? []) : []),
  };
}
