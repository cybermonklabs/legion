import type { BlockPin, GraphEdge, GraphNode } from "./dispatch-api.generated";
import type { DisplayTree } from "./dispatch-api-core";

/**
 * The reference graph: what points at what, read by `GET /api/v1/references` and
 * `GET /api/v1/block-links`, and the document lists built on it.
 */

// GraphEdgeKind is an edge's kind: one of the structural kinds, or the name of a typed block's
// `block[]` attribute (BlockAttributeSchema), whose pointers are edges of that kind. The block
// schema decides those names (Go's pmdoc.LinkKinds), so any other string is admitted beside the
// structural ones, which keep their completion; none of them is ever an attribute's name. The
// route table widens the edge's kind to a plain string, so the completion is spelled here.
export type GraphEdgeKind =
  | "mentions"
  | "child_of"
  | "blocked_by"
  | "attached_to"
  | "anchored_to"
  | "owned_by"
  | "replies_to"
  | "answers"
  | "followed_by"
  | "part_of"
  | "depends_on"
  | "affects"
  | (string & {});

// BlockPinWithTrees is a pinned block reference as the web app reads it, with the tree of the
// text the pinned version held. The pin itself stands on a node a shared row carries, and gains
// no field; a tree rides on this reading of it.
export type BlockPinWithTrees = BlockPin & {
  readonly text_tree?: DisplayTree;
};

// GraphNodeWithTrees is one end of an edge as the web app reads it: the node, the tree of its
// own text, and its pin carrying the tree of the text that version held. A reference's title is
// read from one of those trees. Every tree is absent unless the request sent
// `Dispatch-Trees: 1`, so this reading is a `GraphNode` wherever one is wanted.
export type GraphNodeWithTrees = Omit<GraphNode, "pin"> & {
  readonly text_tree?: DisplayTree;
  readonly pin?: BlockPinWithTrees;
};

// GraphReferencesWithTrees is GET /api/v1/references as the web app reads it: the queried node
// carrying its trees, with the edges, which hold no stored text of their own.
export interface GraphReferencesWithTrees {
  readonly node: GraphNodeWithTrees;
  readonly edges: GraphEdge[] | null;
}
