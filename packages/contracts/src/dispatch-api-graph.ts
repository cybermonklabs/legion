import type { GraphEdgeWithTrees, GraphExcerptWithTrees } from "./dispatch-api.generated";

/**
 * The reference graph: what points at what, read by `GET /api/v1/references` and
 * `GET /api/v1/block-links`, and the document lists built on it.
 */

/** An edge's excerpt without the tree a dashboard read carries: the excerpt the route table holds
 *  (`GraphExcerptWithTrees`) less its `text_tree`. It is written here because the route table no
 *  longer names the plain excerpt: the server's edge read declares the edge's own fields rather
 *  than embedding model.GraphEdge, so the document's reflector never reaches model.GraphExcerpt,
 *  and main exports this name. A `GraphExcerptWithTrees` is a `GraphExcerpt` wherever one is
 *  wanted. */
export type GraphExcerpt = Omit<GraphExcerptWithTrees, "text_tree">;

/** One edge of the reference graph without the tree a dashboard read carries: the edge the route
 *  table holds (`GraphEdgeWithTrees`) with its excerpt back to the plain `GraphExcerpt`. A
 *  `GraphEdgeWithTrees` is a `GraphEdge` wherever one is wanted. */
export type GraphEdge = Omit<GraphEdgeWithTrees, "excerpt"> & { readonly excerpt?: GraphExcerpt };

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
