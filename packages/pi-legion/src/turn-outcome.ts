import { messageFor } from "@legion/envoy-client/errors";
import { type LegionDaemonClient, retryWhileDaemonRestarts } from "@legion/pi-shared/daemon-client";
import type { ClaimCapability } from "@legion/pi-shared/legion-phase-ops";
import { assistantRefusal } from "@legion/pi-shared/phase-stall";
import type { AgentEndEvent } from "@legion/pi-shared/pi-types";

/**
 * The Oh My Pi half of the turn-outcome report (INFRA-714). Every turn this pane ends — one the
 * model answered, and one the provider refused — is reported to the daemon over HTTP, under a
 * sequence this plugin counts itself. The daemon derives the claim's run of refused turns from
 * the reports, and the run fails no claim (dispatch://INFRA-4508): it decides only whether a retry
 * of the claim starts a new session rather than resuming the one every recent turn of which was
 * refused (`freshSession`, `packages/daemon/internal/supervise/turnoutcome.go`).
 *
 * The count is this producer's alone. It is not the daemon's turn ordinal and not
 * `supervise.StreamTurnEnd`'s: two counters would drift the moment one of them missed an end, and
 * only one of them can own the number a report is keyed by. It starts at 1 within a claim's
 * launch generation, which a pane holds for its whole life, and is taken before the report is
 * sent, so reports that overlap keep the order their turns ended in whatever order they arrive —
 * the daemon keys each outcome by `(generation, sequence)` and derives the trailing run of
 * refusals afresh, so a late report lands in its own place and nothing is ever recounted.
 *
 * Claude Code's half of the same report is `packages/daemon/internal/claudebridge`: neither half
 * is a shimwire frame, because a plugin cannot put one on the shim stream and `shimwire.AgentEnd`
 * carries the whole chunked transcript.
 */

export interface TurnOutcomeLog {
  readonly debug: (message: string, context?: Record<string, unknown>) => void;
  readonly warn: (message: string, context?: Record<string, unknown>) => void;
}

export interface TurnOutcomeReporterDeps {
  /** The pane's daemon client, built lazily by the entry that owns it. */
  readonly daemon: () => LegionDaemonClient;
  /** The claim the session with that id registered, or undefined: a session before its
   * registration, the controller, a person's own pane, and a `task` subagent, which loads an
   * instance of its own and registers nothing. */
  readonly claim: (sessionID: string) => ClaimCapability | undefined;
  readonly log: TurnOutcomeLog;
}

export interface TurnOutcomeReporter {
  /**
   * Reports the turn that `agent_end` just ended, for the claim the session holds. Never rejects:
   * Oh My Pi does not await an `agent_end` handler, so a rejection here would be an unhandled one,
   * and nothing of the turn waits on the report either way.
   */
  readonly report: (sessionID: string, event: AgentEndEvent) => Promise<void>;
}

export function createTurnOutcomeReporter(deps: TurnOutcomeReporterDeps): TurnOutcomeReporter {
  /** The generation the sequence below counts within, and the last number it gave out. */
  let counting: number | undefined;
  let sequence = 0;

  return {
    report: async (sessionID, event) => {
      const claim = deps.claim(sessionID);
      if (claim === undefined) return;
      const refusal = assistantRefusal(
        event.messages?.findLast((message) => message.role === "assistant")
      );
      // A refusal Oh My Pi goes on from ends no turn: the host has already scheduled a retry or the
      // next model of its fallback chain (`willContinue`), and the end of that attempt is the
      // turn's end, reported in its turn. Counted, each fallback step was one more refused turn of
      // the run (dispatch://INFRA-4508). An end that waits only on a background job's result
      // (`awaitingAsyncWork`) has settled, as src/model-fallback.ts reads it, so it is reported; so
      // is an answered end the host goes on from, which ends the run of refusals before it.
      if (
        refusal !== undefined &&
        event.willContinue === true &&
        event.awaitingAsyncWork !== true
      ) {
        return;
      }
      // Taken before the first await, so two turns can never take the same number nor take them
      // out of the order they ended in. A generation this producer has not counted within starts
      // the count again, as a fresh launch's does.
      if (claim.generation !== counting) {
        counting = claim.generation;
        sequence = 0;
      }
      sequence += 1;
      // The number this report owns: `sequence` moves on as the next turn ends, and the lines
      // below are written after an await.
      const mine = sequence;
      const request = {
        claimToken: claim.claimToken,
        sessionId: claim.sessionID,
        secret: claim.secret,
        generation: claim.generation,
        sequence: mine,
        outcome: refusal === undefined ? ("clean" as const) : ("refused" as const),
        ...(refusal === undefined ? {} : { message: refusal }),
      };
      try {
        // A send that fails on nothing but a restarting daemon is retried until it lands: the
        // daemon stores one outcome per `(generation, sequence)`, so a repeat of this report
        // writes the same cell and changes no count, and a turn that goes unreported is a turn
        // the daemon counts neither way — a refusal silently dropped is a run of refusals that
        // comes up short. A refusal the daemon itself gives (a 4xx) will not pass on retry: it is
        // warned about once and the turn goes uncounted, since nothing this pane can do fixes it.
        const outcome = await retryWhileDaemonRestarts("claims/turn-outcome", () =>
          deps.daemon().turnOutcome(request)
        );
        deps.log.debug("legion: reported a turn's outcome", {
          issue: claim.issue,
          role: claim.role,
          sequence: mine,
          outcome: request.outcome,
          refusedRun: outcome.refusedRun,
          failed: outcome.failed,
        });
      } catch (error) {
        deps.log.warn("legion: a turn's outcome did not reach the daemon", {
          issue: claim.issue,
          role: claim.role,
          sequence: mine,
          outcome: request.outcome,
          error: messageFor(error),
        });
      }
    },
  };
}
