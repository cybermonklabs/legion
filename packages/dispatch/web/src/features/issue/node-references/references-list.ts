import type { GraphEdge } from "../../../api/types";
import { isBlockItemKind } from "../../refs/block-reference";
import { firstLineOf } from "../issue-tree";
import { blockWords } from "../tree-drafts";

/**
 * What references one node, as Details draws it (dispatch://INFRA-1196 R14, R19): one row per
 * node that points at the focused one, in the order the graph read answers them, which is
 * newest first.
 *
 * The old page drew this per block of a document, as a disclosure on the Spec, Verification and
 * Plan tabs (`features/refs/ReferencedBy.tsx`'s `BlockReferences` at the merge base): what
 * serves the block, what needs it, and every mention of it. The new page draws `serves` as the
 * spine - a check and a plan task sit under the requirement they serve - and `needs` as a chip
 * (`tree-drafts.ts`'s `servePlacements`), so those two reach the reader from the outline; a
 * mention of a node reaches them nowhere else, which is what this list is for.
 *
 * Pure, so the rows are read without a browser: the component draws them and holds the read.
 */

/** The edge kind that is no tuple of the model: one node's text names another. */
const MENTIONS = "mentions";

/** One node that references the focused one. */
export interface ReferenceRow {
  /** When the reference was made (`GraphEdge.created_at`), which the old panel drew on each
   *  edge (R19). */
  readonly at: string;
  /** What the citing node does for the focused one, as the graph names the edge's kind:
   *  `serves`, `needs`, `answers`, `mentions` and the rest. The component draws the page's
   *  word for it, never this token (R22). */
  readonly kind: string;
  /** The citing node's own first line, as every place that draws one line for a node cuts it
   *  (R1, `firstLineOf`); `""` where the read carries no words of it. */
  readonly line: string;
  /** What kind of node cites, as the graph names it: `block`, `comment`, `ask`, `message`,
   *  `artifact`, `issue`, `component`, `session`. A comment, a message or a question whose block
   *  holds the mention reads as that item. The component draws the page's word for it where the
   *  edge's own kind has none. */
  readonly nodeKind: string;
  /** The citing node's own `dispatch://` address, which the row's link opens; `undefined` for
   *  a node the read names no address for, and such a row is no link. */
  readonly ref: string | undefined;
  /** The citing node itself, which is what makes it one row however many edges it holds. */
  readonly key: string;
  /** The issue the citing node belongs to, or its project where it belongs to none, which the
   *  component draws where the citing node is not of the focused node's own issue (the old
   *  panel's "Comment · LAY-9"); `undefined` where the read names neither. */
  readonly source: string | undefined;
}

/**
 * The words the read carries for the citing node: the excerpt the server sends for a node that
 * has text of its own, which for a block of a document is that block's stored excerpt
 * (`packages/envoy/internal/dispatch/refs/graph.go:250-258`), else the node's own text, with a
 * typed block's directive line and closing fence taken off as every chip's target is named
 * (`tree-drafts.ts`'s `blockWords`): both are markup and hold a block id and a slug, which the
 * page draws nowhere a word belongs (R22).
 */
export function nodeWordsOf(edge: GraphEdge): string {
  return blockWords(edge.excerpt?.text ?? edge.node.text ?? "");
}

/**
 * The ask, comment or message a graph node is a block of: its parent, for a block node naming
 * no document (`ask/<uuid>#b2`, `comment/<uuid>#b2`, `message/<uuid>#b1`); undefined for any
 * other node. The graph resolves a mention inside an item's words to that item's block
 * (`refs/item_block_nodes.go`), and hides the item's own mention whenever one of its blocks
 * holds the link, so this is how a comment that mentions the row reads as a comment and not as
 * a document (`features/refs/ReferencedBy.tsx`'s `blockItem`, ported).
 */
function blockItem(node: GraphEdge["node"]): NonNullable<GraphEdge["node"]["parent"]> | undefined {
  return node.kind === "block" &&
    node.document === undefined &&
    node.parent !== null &&
    isBlockItemKind(node.parent.kind)
    ? node.parent
    : undefined;
}

/**
 * One row per citing node, in the read's own order.
 *
 * A node that cites the focused one twice - a comment that names it in two of its blocks, a
 * check that both serves it and names it in its words - is one row, because the row is a way
 * to that node and the node is one place: a block of a comment, a message or a question is
 * that item, keyed and named by it. Which of its edges names the row is the one that says the
 * most: a tuple of the model over a mention, since "served by" tells the reader what the node
 * does and a mention tells them only that it was named. The row keeps the place of the first
 * edge either way, so the list stays in the order the read answered.
 */
export function referenceRows(edges: readonly GraphEdge[]): ReferenceRow[] {
  const rows = new Map<string, ReferenceRow>();
  for (const edge of edges) {
    const item = blockItem(edge.node);
    const nodeKind = item?.kind ?? edge.node.kind;
    const key = `${nodeKind}:${item?.id ?? edge.node.id}`;
    const held = rows.get(key);
    if (held !== undefined && (held.kind !== MENTIONS || edge.kind === MENTIONS)) continue;
    const words = nodeWordsOf(edge);
    rows.set(key, {
      at: held?.at ?? edge.created_at,
      key,
      kind: edge.kind,
      line: words === "" ? "" : firstLineOf(words).line,
      nodeKind,
      ref: item?.ref ?? edge.node.ref,
      source: edge.node.issue_key ?? edge.node.project,
    });
  }
  return [...rows.values()];
}
