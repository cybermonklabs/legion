import type { Agent } from "../../../api/types";
import { compareTimestamps } from "../../../lib/timestamps";
import type { NeedsYouBySession } from "../ask-tally";

/**
 * Pinned sessions first, in pin order; then whoever needs the viewer, then the most open asks,
 * then the newest Dispatch activity (sessions Dispatch never heard from last), then the title.
 */
export function orderAgents(
  agents: readonly Agent[],
  pinned: readonly string[],
  needsYou: NeedsYouBySession
): Agent[] {
  const pinOrder = new Map(pinned.map((sessionID, index) => [sessionID, index]));
  return [...agents].sort((left, right) => {
    const leftPin = pinOrder.get(left.session_id);
    const rightPin = pinOrder.get(right.session_id);
    if (leftPin !== undefined || rightPin !== undefined) {
      if (leftPin === undefined) return 1;
      if (rightPin === undefined) return -1;
      return leftPin - rightPin;
    }
    const byNeedsYou = (needsYou.get(right.session_id) ?? 0) - (needsYou.get(left.session_id) ?? 0);
    if (byNeedsYou !== 0) return byNeedsYou;
    if (left.open_asks !== right.open_asks) return right.open_asks - left.open_asks;
    if (left.last_activity === null || right.last_activity === null) {
      if (left.last_activity === null && right.last_activity !== null) return 1;
      if (left.last_activity !== null && right.last_activity === null) return -1;
    } else {
      const byActivity = compareTimestamps(right.last_activity, left.last_activity);
      if (byActivity !== 0) return byActivity;
    }
    return left.title.localeCompare(right.title) || left.session_id.localeCompare(right.session_id);
  });
}
