import { type ReactNode, useMemo } from "react";
import { Link } from "react-router-dom";

import { pageQuietText } from "../../theme/classes";
import { usePlainHeadline } from "../refs/plain-text";
import { parseNodeReference, referenceHref } from "../refs/routes";
import { drawnLine, firstLineOf, lineReading, type ParentLine, type TreeNode } from "./issue-tree";
import { authorText } from "./page-structure";
import { NODE_TYPE_NAMES, type NodeType } from "./page-words";

/**
 * A node's one line (R1), as every place that draws one draws it - a folded branch's line
 * (`FirstLine`), a parent's line under a filter (`ParentLink`) and a crumb above a zoom
 * (`IssueTree.tsx`'s `CrumbName`): its node's first line, projected from its words by the shared
 * plain-text projection (`lineReading`) and cut by R1's first-line rule (`firstLineOf`), with an
 * ellipsis only where the cut ended inside a sentence (`drawnLine`), so a heading never runs into
 * the paragraph after it. A reference in it reads as the same reference in the node's row does
 * (`RowText.tsx`): as its target's current name once it resolves, and as the words its author
 * wrote until then (R8, INFRA-1196 decision 350); and a numbered step keeps its number (decisions
 * 66 and 88). The issue's own line is its key and then its projected title, the key handed to
 * R1's rule apart from the title, which reads a label after it (`LineReading.key`). Each role is
 * read by the issue the node's record is on. `line` is `undefined` until the projection has
 * answered, which is one pre-paint commit on a page whose engine is loaded, and `""` for a node
 * whose words hold none. `reads` draws nothing and the caller mounts it where it draws the line.
 */
export function useNodeLine(
  node: Pick<TreeNode, "id" | "source" | "text" | "tree" | "type">,
  {
    surface,
  }: { readonly surface: "node-row-first-line" | "node-row-parent-line" | "outline-crumb" }
): { line: string | undefined; reads: ReactNode } {
  const { id, source, text: stored, tree, type } = node;
  const reading = useMemo(
    () => lineReading({ id, text: stored, tree, type }),
    [id, stored, tree, type]
  );
  const { reads, text } = usePlainHeadline(
    "tree" in reading ? reading.tree : undefined,
    "markdown" in reading ? reading.markdown : undefined,
    { firstLine: "markdown" in reading, issue: source?.issue, surface }
  );
  const words = "none" in reading ? "" : text?.text;
  const key = "key" in reading ? reading.key : undefined;
  return {
    line: words === undefined ? undefined : drawnLine(firstLineOf(words, key)),
    reads,
  };
}

/** The words a place that names a node by its one line draws where it must name it by
 *  something - a parent's line under a filter (`ParentLink`) and a crumb above a zoom
 *  (`IssueTree.tsx`'s `CrumbName`): the line (`useNodeLine`), and the page's word for the node's
 *  kind where its words hold none (R22). Nothing until the projection has answered. */
export function lineOrKindWord(line: string | undefined, type: NodeType): string {
  return line === "" ? NODE_TYPE_NAMES[type] : (line ?? "");
}

/** A folded branch's one line (`useNodeLine`): empty until the projection has answered. */
export function FirstLine({ node }: { node: TreeNode }): ReactNode {
  const { line, reads } = useNodeLine(node, { surface: "node-row-first-line" });
  return (
    <span data-node-first-line="" {...authorText}>
      {reads}
      {line ?? ""}
    </span>
  );
}

/** The parent's own first line, as every one line of a node is drawn (`useNodeLine`), and the
 *  page's word for its kind where its words hold none (R22): as the link a reader follows to
 *  that node where the model gives it an address (R18, and R26: a step's row reaches the
 *  statement it serves), and as words with no link where that address names a person, whom no
 *  page shows (R5). Out of the tab order, as every other control inside a row is, unless
 *  `tabStops` holds: `f` steps to it and Enter follows it. */
export function ParentLink({ line, tabStops }: { line: ParentLine; tabStops: boolean }): ReactNode {
  const parent = line.reference === undefined ? undefined : parseNodeReference(line.reference);
  const href = parent === undefined ? undefined : referenceHref(parent);
  const { line: drawn, reads } = useNodeLine(line, { surface: "node-row-parent-line" });
  // Nothing until the projection answers, one pre-paint commit on a page whose engine is loaded.
  const words = lineOrKindWord(drawn, line.type);
  return href === undefined ? (
    <span className="min-w-0 break-words" {...authorText}>
      {reads}
      {words}
    </span>
  ) : (
    <Link
      className={`dispatch-inline-link min-w-0 break-words underline decoration-page-bullet underline-offset-[3px] ${pageQuietText}`}
      tabIndex={tabStops ? undefined : -1}
      to={href}
      {...authorText}
    >
      {reads}
      {words}
    </Link>
  );
}
