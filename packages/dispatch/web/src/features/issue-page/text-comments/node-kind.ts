import { compiledBlockSchema } from "../../doc/schema";
import type { TreeAnswerNode } from "../tree-read/tree-read";

/** The names of the schema's typed blocks (`pmdoc.IsTypedBlock`). */
const TYPED_BLOCKS: ReadonlySet<string> = new Set(
  compiledBlockSchema.types.map(({ name }) => name)
);

/**
 * `node`'s kind as the server's tree read tells it apart (`kind=`, `treeKinds`): a port of the
 * server's `issueTreeBuilder.nodeKind` (`api/issue_tree_kinds.go`), its branches in its order, and
 * the copy of that rule the margin's write (`subjectOf`) and its read (`conversationKindOf`, which
 * the Contents list reads) take; the page's drawing (`lensTypeOf` in `lenses/lens-drafts.ts`,
 * `isPlainBlock` in `tree-read/tree-nodes.ts`) keeps a parent-based rule of its own: a block moved
 * under a lens is a finding there. A node whose store kind is not `block` is of its store kind. A
 * block whose id names its lens (`lens/<uuid>#<block id>`, as `graphstore.ParseFindingID` reads
 * it) is a finding, whatever its supertag and its parent. A block whose supertag is a typed block
 * of the schema (`pmdoc.IsTypedBlock`; `compiledBlockSchema`, the manifest the server of the same
 * build embeds) is of that type: the tree read answers a block's supertag as `refs.TreeNodeType`,
 * the typed block its text opens. Every other block is prose, a `lens_block` whose id names no
 * lens among them. The one exception to the port is an ask's block, which the server gives no
 * kind and which is `ask` here.
 */
export function nodeKindOf(node: Pick<TreeAnswerNode, "id" | "kind" | "supertag">): string {
  if (node.kind !== "block") return node.kind;
  if (node.id.startsWith("lens/")) return "finding";
  // The server gives an ask's block no kind, since `kind=ask` takes the ask's own node. The page
  // draws an ask's block as the ask's own row (`draftAsk`; `treeIdOfNode` gives the ask its
  // block's tree id, `tree-read/tree-nodes.ts:413`), so a comment on the question's words is a
  // comment on the ask, and it keeps its fallback.
  if (node.supertag === "ask") return "ask";
  if (TYPED_BLOCKS.has(node.supertag)) return node.supertag;
  return "prose";
}
