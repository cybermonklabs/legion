import type { ReferenceTarget } from "../../../api/types";
import { buildDispatchReference, type IssueRoute } from "../../refs/routes";
import type { IssueAddress } from "../issue-address";
import type { TreeNode } from "../issue-tree";
import { answerHolds } from "../presets";
import { lastHeldOf } from "../tree-drafts";
import { nextTreeCursor } from "../tree-read/tree-nodes";
import type { TreeAnswer } from "../tree-read/tree-read";
import { lastHeldPath } from "./last-held";

/**
 * What the page asks `reference-targets` for where an address names, by its `#b-` fragment, a
 * block the read its view draws from lacks, and where the address opens by the answer: the Spec
 * view's own address through the page's names read (`useIssueTree.ts`), and a document's own
 * address through `address-target.ts`' `useLastHeldAddress`. It imports neither of those two
 * modules, so both import it and no import cycle closes through it.
 */

/** The reference a `reference-targets` read is asked for the block `address` names by the
 *  explicit `#b-` fragment of the address (`hash`), pinning no version: on the Spec view's own
 *  address the spec's block, and on a document's own address that document's
 *  (`buildDispatchReference`). Undefined for every other address: one that names no block, or
 *  names it by no fragment, one that pins a version, as the address `last_held` opens does, and
 *  one whose route names no document, Graph's among them (`/graph/<node id>`, decision 456). */
export function lastHeldReference(
  route: IssueRoute,
  address: Pick<IssueAddress, "target" | "version">,
  hash: string
): string | undefined {
  const target = address.target;
  if (!hash.startsWith("#b-") || target?.kind !== "block" || address.version !== undefined) {
    return undefined;
  }
  const block = { id: target.id };
  if (route.kind === "spec") return buildDispatchReference({ block, key: route.key, kind: "spec" });
  if (route.kind !== "artifact" || route.version !== undefined) return undefined;
  return buildDispatchReference({ block, key: route.key, kind: "artifact", slug: route.slug });
}

/**
 * The reference of the block an address names by its explicit `#b-` fragment
 * (`lastHeldReference`) where the read its view draws from conclusively lacks the block (R9 and
 * R12 of Graph editing): the one the page then asks `reference-targets` for, to open the version
 * that last held the block (`lastHeldOpening`). Undefined for every other address, and while
 * the read does not show the block absent.
 *
 * `held` is the read the address's view draws from, with every page of it read so far: the Spec
 * view's own read once held, else the outline read, as `presetView` draws that view
 * (`useIssueTree`), and the outline read for a document's address, at which the outline is drawn
 * (`slots/body.tsx`). It shows the block absent only once it holds an answer that names no page
 * more (`nextTreeCursor`) - until then the block may stand on a page not read yet, or in an
 * answer still out - and where neither that answer (`answerHolds`) nor the tree the page draws
 * from the reads (`reveal`, `pathToTarget`) holds it, so an address of a block a read holds
 * lands as it does today.
 */
export function lastHeldAsk({
  address,
  hash,
  held,
  reveal,
  route,
}: {
  readonly address: Pick<IssueAddress, "target" | "version">;
  readonly hash: string;
  /** The read the address's view draws from; undefined while it holds no answer. */
  readonly held: TreeAnswer | undefined;
  /** The chain the tree holds down to the address's node (`pathToTarget`). */
  readonly reveal: readonly TreeNode[] | undefined;
  readonly route: IssueRoute;
}): string | undefined {
  const reference = lastHeldReference(route, address, hash);
  const target = address.target;
  if (reference === undefined || target?.kind !== "block" || reveal !== undefined) return undefined;
  if (held === undefined || nextTreeCursor(held) !== undefined) return undefined;
  return answerHolds(held, target) ? undefined : reference;
}

/**
 * Where an address opens that names a block the read its view draws from lacks
 * (`lastHeldAsk`), by the member `reference-targets` answered for the block: where it answers the
 * block gone, the version that last held it, at the page's own address of it (`lastHeldOf`), and
 * with no version the page can read, `unblocked` (`lastHeldPath`). Undefined where it answers the
 * block live, or answers nothing of it: the address stands as it is, so a live block no tree read
 * holds - a plain paragraph of a document - opens as today.
 */
export function lastHeldOpening(
  member: ReferenceTarget | undefined,
  unblocked: string
): string | undefined {
  return member?.gone === true ? (lastHeldPath(lastHeldOf(member)) ?? unblocked) : undefined;
}
