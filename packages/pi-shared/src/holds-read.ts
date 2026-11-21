import type { LegionTask } from "@legion/contracts/legion-api";
import type { ClaimCaller, LegionDaemonClient } from "./daemon-client";

/**
 * The read that tells a claim's worker of the takes it holds (dispatch://INFRA-4130 R2), the same
 * on every harness: Oh My Pi's claim session reads it at a tool result of its own agent while a
 * run is going (`packages/pi-legion/src/claim-session.ts`), and Claude Code's phase hook at a
 * `PostToolUse` of the worker's own (`packages/claude-envoy/src/legion-phase-hook.ts`). It is the
 * claim's own `claims/task`, with the claim's caller, of which only `holds` is kept: the claim's
 * own value reaches its take from the registration, the relay and the take's own reads, so this
 * read hands it nothing. Each host keeps its own gate on when it calls, and its own log.
 */

/** A take of another claim of the issue that the daemon holds behind this claim's turn: that
 * claim's role, and its task's number (`holds` of `POST /legion/v1/claims/task`). */
export type HeldTake = NonNullable<LegionTask["holds"]>[number];

/** How long after a holds read started the next may start: R2's own bound of a minute. */
const HOLDS_READ_EVERY_MS = 60_000;

/** How long a holds read is waited for: the request is aborted then, and the read names
 * nothing. */
export const HOLDS_READ_BOUND_MS = 3_000;

/** A claim's holds read (`createHoldsRead`). */
export interface HoldsRead {
  /**
   * The takes the latest holds read of this turn named that this process has not returned
   * before, each recorded as it is returned, so a take is returned once per role and task; and
   * the start of the next read where one is due: at the first call that has a caller, and at the
   * first call a minute or more after the last read started ({@link HOLDS_READ_EVERY_MS}). The
   * read is not waited for, so its answer is returned from the first call after it. Empty, and
   * starting no read, while there is no caller.
   */
  readonly holds: () => readonly HeldTake[];
  /** The holder's turn ended: what this turn's reads named is dropped, and so is the answer of
   * a read still unanswered, since the daemon accepts each take they name at that end. */
  readonly turnEnded: () => void;
  /**
   * A turn of the holder started: the answer an earlier read gave is dropped where that read
   * started a minute or more ago ({@link HOLDS_READ_EVERY_MS}), as is the answer a turn that
   * ended with no `turnEnded` (an interrupted one) kept. A fresher answer is kept, and so is a
   * read still unanswered, its answer returned by the first call after it comes. Claude Code's
   * phase hook calls it at `UserPromptSubmit`, beside `turnEnded` at `Stop` and `StopFailure`
   * (`packages/claude-envoy/src/legion-phase-hook.ts`). Claude Code runs `Stop` before a step's
   * `result`, and the bridge writes the next step's user message only after that `result`
   * (`sendQueued` in `packages/daemon/internal/claudebridge/bridge.go`), so after a `Stop`
   * nothing is left to keep: what this keeps comes from a turn that ended with neither, a race
   * rather than the common path. Its cost is one note, from an answer whose read started less than
   * a minute before, for a take that has already started. It does not move the count of turns
   * `turnEnded` moves.
   */
  readonly turnStarted: () => void;
}

/** The claim's holds read: a read is bounded at {@link HOLDS_READ_BOUND_MS}, and one that fails
 * or is bounded out names nothing and is handed to `failed`. */
export function createHoldsRead(deps: {
  readonly daemon: Pick<LegionDaemonClient, "task">;
  /** The claim's caller, or undefined while there is no claim to read for. */
  readonly caller: () => ClaimCaller | undefined;
  /** The clock the reads are a minute apart by. */
  readonly now: () => number;
  /** A read failed, or had no answer within {@link HOLDS_READ_BOUND_MS}: the host's log. */
  readonly failed: (error: unknown) => void;
}): HoldsRead {
  let lastRead: number | undefined;
  // Counts the holder's turns, so that an answer that comes after its turn ended is dropped.
  let turn = 0;
  // The latest answer of this turn's reads, whose `holds` are read off it at each call, and the
  // time the read that brought it started, by which `turnStarted` tells an answer it drops.
  let latest: LegionTask | undefined;
  let latestStarted: number | undefined;
  // The role and task of each take this process has returned.
  const returned = new Set<string>();
  return {
    holds: () => {
      const caller = deps.caller();
      if (caller === undefined) return [];
      const due = (latest?.holds ?? []).filter(
        (take) => !returned.has(`${take.role} ${take.task}`)
      );
      for (const take of due) returned.add(`${take.role} ${take.task}`);
      const now = deps.now();
      if (lastRead === undefined || now - lastRead >= HOLDS_READ_EVERY_MS) {
        lastRead = now;
        const during = turn;
        void deps.daemon
          .task(caller, { signal: AbortSignal.timeout(HOLDS_READ_BOUND_MS) })
          .then((reading) => {
            if (turn !== during) return;
            latest = reading;
            latestStarted = now;
          }, deps.failed);
      }
      return due;
    },
    turnEnded: () => {
      turn += 1;
      latest = undefined;
    },
    turnStarted: () => {
      if (latestStarted !== undefined && deps.now() - latestStarted >= HOLDS_READ_EVERY_MS) {
        latest = undefined;
      }
    },
  };
}
