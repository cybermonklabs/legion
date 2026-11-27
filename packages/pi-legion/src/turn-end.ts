import type { AgentEndEvent } from "@legion/pi-shared/pi-types";

/**
 * Whether Oh My Pi goes on with the turn an `agent_end` ended, itself, so the turn has not
 * settled: the host has already scheduled a retry, the next model of its fallback chain or a
 * hook's continuation (`willContinue`). An end that waits only on a background job's result
 * (`awaitingAsyncWork`) has settled: the host reaches it only after each of its own retries and
 * continuations has declined the turn (`session/agent-session.ts:6477-6480` at the pinned tag),
 * and the job's result starts the next turn. A steered run's non-terminal end sets neither flag
 * (`AgentEndEvent`), so it reads as settled here.
 *
 * Both readers of a settled turn ask this one question, so they never disagree about what a turn
 * is: the model fallback, which would step the session down twice (`./model-fallback`), and the
 * turn-outcome report, which would count each refused step of a fallback walk as one more refused
 * turn (`./turn-outcome`).
 */
export function turnGoesOn(event: AgentEndEvent): boolean {
  return event.willContinue === true && event.awaitingAsyncWork !== true;
}
