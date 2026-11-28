/**
 * The press of the header's count of what waits on the reader. What the count counts is
 * Dashboard's items (`features/dashboard/useWaitingItems.ts`,
 * dispatch://INFRA-5702/spec#b-r-header-count); this module holds what a press leaves for the
 * Decisions filter to read.
 */

/** What the count leaves in the address's history entry, which the Decisions filter reads to put
 *  the focus on what waits once it has drawn it: one press, told apart from the next by
 *  `waitingPress`, so each press goes on to the next item that waits. */
export interface WaitingPress {
  readonly waitingPress: number;
}

/** The press `state` carries, where the history entry is the count's own. */
export function waitingPressOf(state: unknown): number | undefined {
  if (typeof state !== "object" || state === null || !("waitingPress" in state)) return undefined;
  const press = state.waitingPress;
  return typeof press === "number" ? press : undefined;
}
