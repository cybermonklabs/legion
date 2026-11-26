import type { DecisionEntry } from "../../api/types";
import { type TextSource, textSourceOf } from "../refs/text-references";

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

/**
 * The ask a person's decision is, by the ask's id: an answered ask's entry names it as its own
 * node. Undefined for an agent's decision, whose block is its own node. Every view that draws a
 * person's decision at its ask - the tree's node and its `shaped` edge, the Decisions filter's
 * entry - reads the ask here, so they cannot disagree.
 */
export function personDecisionAsk(entry: Pick<DecisionEntry, "kind" | "node">): string | undefined {
  if (entry.kind !== "person") return undefined;
  return entry.node.kind === "ask" ? entry.node.id : undefined;
}

/**
 * The record an entry's words are, which a `#id` in them names a question block of. An ask's
 * entry is the ask's own text, its question and its answer, which the server reads against the
 * document the ask's block is on (`refs.ScopeOf`): the Decisions view names no document for an
 * ask's node, nor for a decision of a document that plays no role, so the ask itself is what
 * names that document. Any other entry is a block of the document it is written in, `documentId`
 * (`decision-review.ts`'s `decisionDocumentId`), and names no record where that is not known.
 * The Decisions tab and the margin's decision cards both read the record here, so they cannot
 * disagree.
 */
export function entryTextSource(
  entry: Pick<DecisionEntry, "node">,
  documentId: string | undefined,
  issueKey: string | undefined
): TextSource | undefined {
  return entry.node.kind === "ask"
    ? textSourceOf("ask", entry.node.id, issueKey)
    : textSourceOf("artifact", documentId, issueKey);
}
