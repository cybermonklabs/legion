import type { ThreadComment } from "../../margin/useMarginItems";
import { buildDispatchReference } from "../../refs/routes";
import type { TreeNode } from "../issue-tree";

/**
 * The node of the outline that stands for one comment or message of a thread on text (R7, R8):
 * what part W's Lens act is invoked on, by the address the node carries (`LensMenu`), and what the
 * Contents list draws an entry of (`contents-entries.ts`). The thread's items are no row of any
 * view (R1), so no tree holds a `TreeNode` for them; this is the one that the margin builds, from
 * the comment as the card draws it, with the fields those two readers use: its address, its words
 * and their reading, its author and its time. `key` names the issue where the comment names none.
 */
export function threadCommentNode(
  comment: ThreadComment,
  kind: "comment" | "message",
  key: string
): TreeNode {
  return {
    alsoUnder: [],
    at: comment.created_at,
    author: comment.author,
    children: [],
    chips: [],
    facts: [],
    gaps: [],
    held: 0,
    id: comment.id,
    question: false,
    reference: buildDispatchReference({ id: comment.id, key: comment.issue_key ?? key, kind }),
    replaced: false,
    text: comment.body,
    transcluded: false,
    ...(comment.body_tree === undefined ? {} : { tree: comment.body_tree }),
    type: kind,
    waiting: false,
  };
}
