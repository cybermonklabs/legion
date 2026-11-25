import type { HandoffSummary } from "../../api/types";
import type { TrackStep } from "./phase-track";

/**
 * The rounds the phase track draws on its counted steps (dispatch://INFRA-5702/spec#b-r-track):
 * the issue's handoffs (`GET /api/v1/issues/{key}/handoffs`, read under `["handoffs", key]`, the
 * key `conversation/ConversationTab.tsx` reads them under) counted by phase. Pure: it counts what
 * the read holds and lists no handoff.
 */

/** The steps that carry a count of rounds, in the track's order. Admitted, Accepting, Retro and
 *  Merging count none. */
const COUNTED_STEPS = [
  "planning",
  "implementing",
  "testing",
  "reviewing",
] as const satisfies readonly TrackStep[];

type CountedStep = (typeof COUNTED_STEPS)[number];

/** The handoff phase each counted step counts (`api/handoffs.go`: Legion writes `plan`,
 *  `implement`, `test` and `review`). */
const ROUND_PHASES: Readonly<Record<CountedStep, string>> = {
  implementing: "implement",
  planning: "plan",
  reviewing: "review",
  testing: "test",
};

/** The number of rounds of each counted step that has at least one: a counted step with no
 *  handoff has no count, so the track draws none for it. */
export function roundCounts(
  handoffs: readonly Pick<HandoffSummary, "phase">[] | undefined
): Readonly<Partial<Record<CountedStep, number>>> {
  const counts: Partial<Record<CountedStep, number>> = {};
  for (const step of COUNTED_STEPS) {
    const rounds = (handoffs ?? []).filter(({ phase }) => phase === ROUND_PHASES[step]).length;
    if (rounds > 0) counts[step] = rounds;
  }
  return counts;
}
