import { compareTimestamps } from "../../lib/timestamps";

/** Where an entry stands in the history, newest first: when it happened, for one instant its
 *  place in the log (`buildHistory`), and for one place its id - every thread stands at place 0,
 *  as the log read holds no thread's first turn, and so does every run of a step, which no event
 *  of the log dates. The panel draws the history down to the place of the last entry it draws, so
 *  an entry that arrives above that place is drawn and takes none of the entries drawn out. */
export interface HistoryPlace {
  readonly at: string;
  readonly id: string;
  readonly seq: number;
}

/** How `one` stands to `other` in the history: above it (more than 0) where it is newer, at the
 *  same place (0), or under it (less than 0). */
export function comparePlaces(one: HistoryPlace, other: HistoryPlace): number {
  return (
    compareTimestamps(one.at, other.at) ||
    one.seq - other.seq ||
    (one.id < other.id ? -1 : one.id > other.id ? 1 : 0)
  );
}
