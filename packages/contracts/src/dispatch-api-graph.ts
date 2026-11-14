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
