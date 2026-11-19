import type { Artifact, IssueDetails } from "../../api/types";

/**
 * Which artifact of the issue each node of the tree belongs to (dispatch://INFRA-1196 R30,
 * `dispatch://INFRA-1196/artifact/plan-md#b-t17`).
 *
 * A node's id is its own address inside the issue: a block of a document is
 * `<slug>#b-<block id>` (`tree-drafts.ts`'s `blockNodeId`). So the document a row's caret goes
 * into, and the block inside it, are read from the row's own id and the issue's artifact list,
 * with no further read.
 *
 * This module is pure. It holds no React and makes no request.
 */

/** Each artifact of the issue by its slug, which is how every block node of the tree names one. */
export function artifactsBySlug(issue: IssueDetails): ReadonlyMap<string, Artifact> {
  return new Map((issue.artifacts ?? []).map((artifact) => [artifact.slug, artifact]));
}

/** One node's place in a document: the document itself, and the block of it the node is. */
export interface NodeDocument {
  readonly artifact: Artifact;
  readonly blockId: string;
}

/** The document `nodeId` is a block of, or `undefined` for a node that is no block: the issue
 *  itself, a comment, a message, an ask of its own, or a result. A document is no node of the
 *  tree (R3), so every node of one is a block of it. */
export function documentOfNode(
  nodeId: string,
  bySlug: ReadonlyMap<string, Artifact>
): NodeDocument | undefined {
  const cut = nodeId.indexOf("#b-");
  if (cut === -1) return undefined;
  const artifact = bySlug.get(nodeId.slice(0, cut));
  return artifact === undefined ? undefined : { artifact, blockId: nodeId.slice(cut + 3) };
}
