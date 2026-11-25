import type { DecisionEntry } from "../../api/types";

/**
 * The entries of the Decisions read, which every view of an issue's decisions reads: the
 * Decisions tab, the issue page's tree, its Decisions filter and its count of what waits, a
 * decision's Keep and Change, the margin's decision cards and the spec's stale marks. The page
 * reads them from the Decisions view of the issue's tree (`api/decisions-tree.ts`) and holds them
 * under its one query key (`spec-marks.ts`'s `issueDecisionsQuery`).
 *
 * This module is pure: it holds no React and makes no request.
 */

/** Whether an entry of the Decisions read is a decision: an agent's, or an ask a person answered.
 *  An open ask is a question, and a declined one decided nothing, so neither is a decision a view
 *  draws beside the nodes its `shaped` tuples reach. */
export function isDecisionEntry(entry: Pick<DecisionEntry, "answer_state" | "kind">): boolean {
  return entry.kind === "agent" || entry.answer_state === "answered";
}
