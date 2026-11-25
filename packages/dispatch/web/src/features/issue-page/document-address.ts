import { useQuery } from "@tanstack/react-query";
import { useLocation } from "react-router-dom";

import { artifactBlocksQuery } from "../../api/queries";
import type { Artifact, IssueDetails } from "../../api/types";
import { buildIssuePath, type IssueRoute } from "../refs/routes";
import { type IssueAddress, issueAddress } from "./issue-address";
import { NODE_TYPES } from "./page-words";
import { useIssueDetail } from "./useIssueDetail";

/** The document an address opens in place of the outline: the route it is drawn at, and the
 *  artifact itself, undefined for a slug the issue does not hold. */
export interface OpenedAddress {
  readonly artifact: Artifact | undefined;
  readonly route: Extract<IssueRoute, { kind: "artifact" }>;
}

/** The outline address a navigation carried in its state (`NodeMenu.tsx`), if it carried one. */
function outlineOf(state: unknown): string | undefined {
  if (typeof state !== "object" || state === null || !("outline" in state)) return undefined;
  return typeof state.outline === "string" ? state.outline : undefined;
}

/**
 * Whether the address opens its document in place of the outline (dispatch://INFRA-1196 R3,
 * R20). A document's own address opens the document, with one exception: an address naming a
 * block of it that the outline holds as a node - a typed block, which a citation of a check or a
 * plan task names - is where that block now stands, so it opens the outline at that node. A block
 * no node holds - a paragraph of the document's own text - opens the document at the block. Which
 * blocks are typed is the blocks read the outline is built from, so this asks it under the same
 * key and sends nothing of its own; until it answers, the address draws the outline it most
 * likely names.
 */
function useOpensDocument(address: IssueAddress, artifact: Artifact | undefined): boolean {
  const block = address.target?.kind === "block" ? address.target.id : undefined;
  const asksBlocks = address.document !== undefined && block !== undefined;
  const blocks = useQuery({
    ...artifactBlocksQuery(artifact?.id ?? ""),
    enabled: asksBlocks && artifact?.kind === "doc",
  });
  if (address.document === undefined) return false;
  if (!asksBlocks || artifact?.kind !== "doc") return true;
  if (blocks.data === undefined) return blocks.isError;
  const types: readonly string[] = NODE_TYPES;
  return !blocks.data.some((one) => one.id === block && types.includes(one.type));
}

/**
 * The document the address opens in place of the outline (part N's R1, R2, R6), or undefined
 * where it opens none. Only an address with no view on opens one: a document's own address,
 * `/artifacts/<slug>` or `/artifact/<slug>`, with what it carries - `?mark=`, a pinned version, a
 * `#b-` of a block no node holds (`useOpensDocument`). An address that keeps a view's preset draws
 * that view, a block of the spec, `?mark=` or a pinned version beside `/spec` among them
 * (`issueAddress`, part I's rule in `slots/stage.tsx`), and so does the outline's own address. The
 * stage draws the document (`slots/stage.tsx`) and the view row's version control reads it
 * (`view-row-tools.tsx`), each through this one rule.
 */
export function useOpenedDocument(
  issue: IssueDetails,
  route: IssueRoute
): OpenedAddress | undefined {
  const { hash, search } = useLocation();
  const { artifactRoute, primaryArtifact, selectedArtifact } = useIssueDetail(issue, route);
  const address = issueAddress(route, search, hash, primaryArtifact?.slug);
  const documentRoute = address.preset === "conversation" ? artifactRoute : undefined;
  const opens = useOpensDocument(address, selectedArtifact);
  return opens && documentRoute !== undefined
    ? { artifact: selectedArtifact, route: documentRoute }
    : undefined;
}

/** The outline the reader goes back to from a document they opened: the address they opened it
 *  from, which the node menu hands over (`NodeMenu.tsx`), and the issue's own otherwise. */
export function useOutlineAddress(key: string): string {
  const { state } = useLocation();
  return outlineOf(state) ?? buildIssuePath({ key, kind: "issue" });
}
