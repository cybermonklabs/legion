import type { Decision, DisplayTree } from "../../api/types";

/**
 * The reading of a decision's heading, taken from the tree Go read its text into: a decision's
 * text runs from its own heading to the end of its section (`api/decisions.go`'s
 * `decisionSections`), so the first heading of that tree is the decision's heading, and a view
 * draws it from there rather than parsing the heading's markdown in the page. `undefined` where
 * the answer carried no reading - a pod of the build before this one during a rolling deploy -
 * and the heading is then parsed as every view falls back to parsing.
 */
export function decisionHeadingTree(
  decision: Pick<Decision, "text_tree">
): DisplayTree | undefined {
  const tree = decision.text_tree;
  const content = tree?.content;
  if (tree === undefined || !Array.isArray(content)) {
    return undefined;
  }
  const heading = (content as DisplayTree[]).find((node) => node.type === "heading");
  return heading === undefined ? undefined : { ...tree, content: [heading] };
}
