import type { AgentEndEvent } from "@legion/pi-shared/pi-types";

/**
 * Whether Oh My Pi goes on with the turn an `agent_end` ended, itself, so the turn has not
 * settled: the host has already scheduled a retry, the next model of its fallback chain or a
 * hook's continuation (`willContinue`). An end that waits only on a background job's result
 * (`awaitingAsyncWork`) has settled: the host reaches it only after each of its own retries and
 * continuations has declined the turn (`session/agent-session.ts:6012-6015` at the pinned tag),
 * and the job's result starts the next turn. A steered run's non-terminal end sets neither flag
 * (`AgentEndEvent`), so it reads as settled here.
 *
 * Every reader of a settled turn asks this one question, so they never disagree about what a turn
 * is: the model fallback, which would step the session down twice (`./model-fallback`), and the
 * `agent_end` entry of `extensions/legion.ts`, which hears no refusal the host goes on from, so
 * the claim's take does not give up its task's number at a refused step of a fallback walk and the
 * turn-outcome report (`./turn-outcome`) does not count each such step as one more refused turn.
 */
export function turnGoesOn(event: AgentEndEvent): boolean {
  return event.willContinue === true && event.awaitingAsyncWork !== true;
}
