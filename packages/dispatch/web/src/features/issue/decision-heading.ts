import { type ReactNode, useMemo } from "react";

import type { DecisionEntry, DisplayTree } from "../../api/types";
import { usePlainHeadline } from "../refs/plain-text";
import type { TextSource } from "../refs/text-references";
import { firstWordedBlock } from "../refs/tree-fragment";
import { decisionName, ownTree } from "./decision-text";

/** What a decision's heading is read from: an entry of the decisions read, or a view's own copy
 *  of one that keeps its question, its words and their reading. */
type HeadedDecision = Pick<DecisionEntry, "question" | "text" | "text_tree">;

/** A decision's heading as written, what a view parses where it holds no reading of it: an ask's
 *  question, else the decision's name, the words of the first block of its own content that
 *  holds any, as the server's read and every view name it (`decisionName`). */
export function decisionHeading(decision: HeadedDecision): string {
  return decision.question ?? decisionName(decision.text_tree, decision.text);
}

/**
 * The reading of a decision's heading, taken from the tree Go read its text into, so a view draws
 * it from there rather than parsing the heading's markdown in the page. An ask's entry holds its
 * question as its text (`decisionread`), so its reading is the question's, and an entry whose
 * text is anything else carries none of its question. A decision is a `decision` block, and the
 * block of its own content that names it is the first that holds any words (`firstWordedBlock`,
 * the block `decisionName` names it by). `undefined` where the answer carried no reading - a pod
 * of the build before this one during a rolling deploy - and the heading is then parsed as every
 * view falls back to parsing.
 */
export function decisionHeadingTree(decision: HeadedDecision): DisplayTree | undefined {
  if (decision.question !== null) {
    return decision.text === decision.question ? decision.text_tree : undefined;
  }
  const tree = ownTree(decision.text_tree);
  return tree === undefined || !Array.isArray(tree.content) ? undefined : firstWordedBlock(tree);
}

/**
 * A decision's heading as plain words, for what it names - a write, a form, a card - and the
 * words of a link to the decision, none of which holds markup (the reading list's
 * `decision-heading-words`): the plain-text projection of the heading's reading
 * (`usePlainHeadline`), with `source` the record the decision is written in. Until the engine has
 * answered, `words` is the heading as written (`decisionHeading`), which is the load state that
 * row accepts, so nothing it names is left with no name. `tree` is the heading's reading, read
 * once per answer of the decisions read, which is when the decision is a new object, for a caller
 * that draws the heading too; `reads` draws nothing, and the caller mounts it wherever it draws.
 */
export function useDecisionHeadingWords(
  decision: HeadedDecision,
  source: TextSource | undefined
): { reads: ReactNode; tree: DisplayTree | undefined; words: string } {
  const headingTree = useMemo(() => decisionHeadingTree(decision), [decision]);
  const heading = useMemo(() => decisionHeading(decision), [decision]);
  const { reads, text } = usePlainHeadline(headingTree, heading, {
    source,
    surface: "decision-heading-words",
  });
  return { reads, tree: headingTree, words: text?.text ?? heading };
}
