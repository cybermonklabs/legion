import type { TreeNode } from "./issue-tree";
import type { DrawnRow } from "./tree-fold";

/**
 * What the tree's keyboard asks of the model. Pure, over the tree `issue-tree.ts` builds and the
 * rows `tree-fold.ts` draws, so the keys hold no reading of the model of their own and a unit
 * test answers the same question the page does.
 */

/** What tells two rows apart: the path from the root down to the node the row draws. A node
 *  drawn under two requirements is two rows, and so is every row below each of them, which is
 *  why the whole path and not the direct parent is the key: the focus, the editing and the
 *  tree's one tab stop belong to the row rather than to the node they are on, and two rows
 *  sharing a key would move together. The row that pages a node's children is keyed by that
 *  node's path, marked as the page's. */
export function rowKeyOf(row: DrawnRow): string {
  const path = [...row.trail, row.kind === "page" ? row.of.id : row.node.id].join(">");
  return row.kind === "page" ? `page:${path}` : path;
}

/** Every node of `root` in reading order, the root first: the order the rows are drawn in, and
 *  the order a key looking for the next node of a kind walks, whatever is folded. */
export function nodesInOrder(root: TreeNode): TreeNode[] {
  const found: TreeNode[] = [];
  const walk = (node: TreeNode): void => {
    found.push(node);
    for (const child of node.children) walk(child);
  };
  walk(root);
  return found;
}

/**
 * The next open question after `from`, in reading order, for a reader standing on one; from
 * anywhere else - no focus at all, or a node that is no question - the first open question of
 * the tree, because a position among nodes that are not questions says nothing about where in
 * the questions the reader is. Undefined when the reader is on the last one, which the key
 * reports rather than wrapping round to a question they have just read (R6).
 */
export function nextQuestion(root: TreeNode, from: string | undefined): TreeNode | undefined {
  const order = nodesInOrder(root);
  const at = order.findIndex((node) => node.id === from && node.question);
  return order.slice(at + 1).find((node) => node.question);
}

/** A reference with the version it pins taken off, so a decision's link to a block and that
 *  block's own address are read as the same node (`dispatch://KEY/spec#b-r1@v3`). */
function withoutVersion(reference: string): string {
  return reference.replace(/@v\d+/g, "");
}

/**
 * The decision behind the node `id`: the first decision of the tree, in reading order, that
 * names that node among its chips. A decision names every block its `shaped` tuples point at
 * (`tree-read/tree-nodes.ts`), and that chip is the one edge the model holds between a decision
 * and what it shaped. An agent's decision is a node of its own; a person's is the answered ask
 * itself, whose node carries those chips.
 *
 * A check no decision names reaches back through the chain it sits in (R10): the decision
 * behind a requirement it is drawn under, which is the decision that informed what the check
 * proves.
 */
export function decisionBehind(root: TreeNode, id: string): TreeNode | undefined {
  const order = nodesInOrder(root);
  const node = order.find((one) => one.id === id);
  if (node === undefined) return undefined;
  const naming = (target: TreeNode): TreeNode | undefined => {
    const addresses = new Set(
      [target.reference, target.id]
        .filter((one): one is string => one !== undefined)
        .map(withoutVersion)
    );
    return order.find(
      (one) =>
        (one.type === "decision" || (one.type === "ask" && one.state === "answered")) &&
        one.chips.some((chip) => addresses.has(withoutVersion(chip.ref)))
    );
  };
  const own = naming(node);
  if (own !== undefined || node.type !== "check") return own;
  for (const requirement of order) {
    if (requirement.type !== "requirement") continue;
    if (!requirement.children.some((child) => child.id === id)) continue;
    const behind = naming(requirement);
    if (behind !== undefined) return behind;
  }
  return undefined;
}
