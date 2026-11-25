import type { Decision, DisplayTree } from "../../api/types";
import { nameBlock, ownTree } from "./decision-text";

/**
 * The reading of a decision's heading, taken from the tree Go read its text into: a decision is
 * named by the first block of its own content that holds any words (`decisionName`, as the
 * server's read of a decision names it), a heading or the paragraph a converted section's heading
 * became, so that block's reading is the heading's, and a view draws it from there rather than
 * parsing the heading's markdown in the page. `undefined` where the answer carried no reading - a
 * pod of the build before this one during a rolling deploy - and the heading is then parsed as
 * every view falls back to parsing, and where no block holds words.
 */
export function decisionHeadingTree(
  decision: Pick<Decision, "text_tree">
): DisplayTree | undefined {
  const tree = ownTree(decision.text_tree);
  const content = tree?.content;
  if (tree === undefined || !Array.isArray(content)) {
    return undefined;
  }
  const name = nameBlock(content as DisplayTree[]);
  return name === undefined ? undefined : { ...tree, content: [name] };
}
