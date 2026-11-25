import type { GraphNode, IssueReferences, ReferenceLink } from "./dispatch-api.generated";

/**
 * The reference graph: what points at what, read by `GET /api/v1/references`,
 * `GET /api/v1/block-links` and an issue's `GET /api/v1/issues/{key}/references`, and the
 * document lists built on it.
 */

// GraphEdgeKind is an edge's kind: one of the structural kinds (Go's model.GraphEdgeKinds, the
// last six of which the store states with no projection row behind them), or the name of a typed
// block's `block[]` attribute (BlockAttributeSchema), whose pointers are edges of that kind. The
// block schema decides those names (Go's pmdoc.LinkKinds), so any other string is admitted beside
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
  | (string & {});

/** `GET /api/v1/issues/{key}/references` as either build of Dispatch answers it ("Two builds at
 *  once", `packages/envoy/cmd/dispatch/README.md`). A build from before the one-hop read answers
 *  `members` and `truncated` alone, so a missing `links` is that build's answer; a `null` list,
 *  which no build writes, holds no row. */
export type IssueReferencesAnswer = Omit<IssueReferences, "links" | "linked_from"> &
  Partial<Pick<IssueReferences, "links" | "linked_from">>;

/** The key that matches a node of one read of the reference graph with a node of another (a row
 *  of the references read, an edge of `GET /api/v1/references`): its ref, or its kind and id
 *  where it has no ref. */
export function graphNodeKey(node: Pick<GraphNode, "kind" | "id" | "ref">): string {
  return node.ref ?? `${node.kind} ${node.id}`;
}

/** An issue's one-hop references as a reader shows them, each node once. */
export interface IssueReferenceSections {
  /** Every row of `links`, in the answer's order, each marked where its node is in `linked_from`
   *  too: a node the issue links to and that links to the issue is shown once, here, as one that
   *  also references the issue. */
  readonly references: readonly { readonly row: ReferenceLink; readonly alsoReferences: boolean }[];
  /** The rows of `linked_from` whose node is not in `links`, in the answer's order. */
  readonly referencedBy: readonly ReferenceLink[];
}

/** The two sections of an answer with `links`; a `null` list counts as one with no row. An answer
 *  without `links` is a build's from before the one-hop read, which a reader shows from its
 *  `members` instead. */
export function issueReferenceSections(
  answer: Pick<IssueReferencesAnswer, "links" | "linked_from">
): IssueReferenceSections {
  const links = answer.links ?? [];
  const linkedFrom = answer.linked_from ?? [];
  const linked = new Set(links.map((row) => graphNodeKey(row.node)));
  const linking = new Set(linkedFrom.map((row) => graphNodeKey(row.node)));
  return {
    references: links.map((row) => ({ row, alsoReferences: linking.has(graphNodeKey(row.node)) })),
    referencedBy: linkedFrom.filter((row) => !linked.has(graphNodeKey(row.node))),
  };
}
