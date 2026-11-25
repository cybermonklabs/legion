import type { PiApi, SessionContext } from "@legion/pi-shared/pi-types";
import type { StallWatchdogAborter } from "@legion/pi-shared/stall-watchdog";

/**
 * The second step of the stall ladder (INFRA-1274, R4): the turn, ended, and the worker's work
 * started again. `packages/pi-shared/src/stall-watchdog.ts` decides when the step runs and writes
 * the message's words (`restartMessage`); this module is the one place that reaches the host for
 * either half of it.
 *
 * **The abort.** `ctx.abort()` ends the turn that runs now, keeps the process and the session,
 * and ends the running call's own child processes. Measured on `omp/18.16.0` in `--mode rpc` on
 * 2026-11-07, from an extension that aborted a never-returning `bash` call: it returns a Promise,
 * the call's result comes back in the same millisecond as `Command aborted` with `isError: true`,
 * the turn ends at once, the command's own children are gone 30 s later while the agent is still
 * alive, and the same process answered an RPC `get_state` and ran a fresh turn 34 s after the
 * abort. So this step costs one turn and nothing else, which is why it runs before the step that
 * costs the process.
 *
 * **`abort` is asked for by `typeof`.** It is present on the handler context but is not an own
 * enumerable property — `Object.keys(ctx)` lists `ui` alone (measured the same day) — so this
 * module asks what the member is, never whether a key list holds it. It is called on the context
 * it came from, since it is the host's method for that session's run.
 *
 * **The message the worker then sends itself.** The daemon counts a task served by the turn that
 * ends and sends nothing after it, so a turn ended here would otherwise leave the worker idle
 * with its work unfinished. The worker puts one message in front of its own session instead,
 * which the host turns into a turn because the session is idle once the abort has ended the turn:
 * the same `pi.sendMessage` delivery the phase reminder starts a turn of a settled worker with
 * (`extensions/legion.ts:189-193`).
 *
 * **It carries its own custom type** and is never sent as a user message: a `role: "user"`
 * message is what the phase check reads as a new assignment (`inboundKind`,
 * `pi-shared/src/phase-stall.ts:172-176`), so a worker restarting itself would reset that check's
 * own count of what it has reminded about.
 *
 * **No message ahead of a pending take** (dispatch://INFRA-4130 R6). The run the abort ended asks
 * the take of an owed task at its `agent_end`, and the take's text, if the daemon accepts it,
 * starts the next run, which is that task's turn; a message of the worker's own would start a turn
 * ahead of it. So where the take is pending when the message would go, after the abort, the
 * message waits for that take, and one line says so; it goes once the take settles with no run
 * starting first (`TaskTake.deferUntilSettled` says when), since an aborted run's `session_stop`
 * arms nothing (`extensions/legion.ts`) and nothing else then starts the work again. It is sent
 * then, once, as it would have been at once. A run that starts first, the take's own or another,
 * is the work going again, and it is not sent.
 *
 * **Both halves fail loudly, and the ladder goes on.** A host with no `abort()`, an `abort()`
 * that throws or whose Promise rejects, and a message the host refuses each throw from here; the
 * detector records what happened and carries the call to the last step at the next gap, rather
 * than leaving the ladder with a turn it ended and nothing to restart it. A held message the host
 * refuses once the take settles has no step left to record it, and the take logs it as a warning.
 */

/** The custom type the restart message carries, beside the phase reminder's
 * `legion-phase-reminder`: a person reading the session tells the worker's own restart from an
 * assignment, and the phase check counts it as neither. */
export const STALL_RESTART_MESSAGE = "legion-stall-restart";

export interface StallAborterDeps {
  /** The handler context the plugin kept from `session_start`, which is where `abort` lives;
   * undefined before the host has handed one over, which ends no turn. */
  readonly context: () => SessionContext | undefined;
  /** `pi.sendMessage`: the host's own delivery, which queues the message and starts a turn on
   * it. */
  readonly sendMessage: PiApi["sendMessage"];
  /** Holds the message's send for the claim's pending take (`ClaimSession.deferUntilTakeSettles`):
   * where the take is pending, it keeps the callback, calls it once the take settles with no run
   * starting first, and answers the number of the task the take waits for
   * (`TaskTake.deferUntilSettled` says when, and which); undefined, keeping nothing, where it is
   * not. */
  readonly deferUntilTakeSettles: (callback: () => void) => number | undefined;
  readonly log: {
    readonly info: (message: string, context?: Record<string, unknown>) => void;
  };
}

/** The detector's `StallWatchdogAborter`: the host's `abort()`, and the one message the worker
 * sends its own session to start the work again. */
export function createStallAborter(deps: StallAborterDeps): StallWatchdogAborter {
  return {
    abortTurn: async (): Promise<void> => {
      const context = deps.context();
      if (context === undefined) {
        throw new Error("this session has no handler context, so it has no turn to end");
      }
      const { abort } = context;
      if (typeof abort !== "function") {
        throw new Error("this host has no ctx.abort(), so the turn could not be ended");
      }
      await abort.call(context);
    },

    sendMessage: (text: string): "sent" | "held" => {
      const send = (): void => {
        deps.sendMessage(
          { customType: STALL_RESTART_MESSAGE, content: text, display: true },
          { deliverAs: "steer", triggerTurn: true }
        );
      };
      const task = deps.deferUntilTakeSettles(send);
      if (task !== undefined) {
        deps.log.info(
          "legion: a stall abort's restart message waits for the take, because a take of the claim's task is pending: the task's text, if the daemon accepts the take, starts the next run, and the message is sent if the take is refused by another hold or dropped, or once a take refused in-turn has waited its bound",
          { task }
        );
        return "held";
      }
      send();
      return "sent";
    },
  };
}
