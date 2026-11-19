/**
 * The reference graph: what points at what, read by `GET /api/v1/references` and
 * `GET /api/v1/block-links`, and the document lists built on it.
 */

// GraphEdgeKind is an edge's kind: one of the structural kinds (Go's model.GraphEdgeKinds, six of
// which the store states with no projection row behind them, and an issue's duplicate_of and
// filed_from, which it holds alone too), or the name of a typed block's `block[]` attribute
// (BlockAttributeSchema), whose pointers are edges of that kind. `needs` is both: an issue's wait
// on an issue, which a Dispatch of the issue tree answers as needs where graph_edges names it
// blocked_by, and a plan task's `block[]` attribute, since waiting is one relation. The block
// schema decides the other names (Go's pmdoc.LinkKinds), so any other string is admitted beside
// the structural ones, which keep their completion; none of them is ever an attribute's name. The
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
  | "changed_by_run"
  | "has_kind"
  | "triggered_by"
  | "constrained_by"
  | "prevented_by"
  | "authored_by"
  | "needs"
  | "duplicate_of"
  | "filed_from"
  | (string & {});
