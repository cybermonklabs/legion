import { isOvertaken, isProposed } from "../issue-address";
import type { NodeChip, NodeLife, TreeNode } from "../issue-tree";
import { withoutVersion } from "../tree-keys";
import { nodeStatesOf } from "../tree-read/tree-nodes";
import type { TreeAnswer } from "../tree-read/tree-read";

/**
 * A node's life and who may read it, on the issue's tree.
 * Pure, over the tree `issue-tree.ts` assembles, which holds neither: the tree read answers each
 * node's state and visibility by the read's own id (`TreeNode.readId`), and `closeOvertaken`
 * puts them on every placement of that node and on every chip and parent line that names it, and
 * closes each overtaken node with every node under it, so the rows, the fold and the counts all
 * read one tree.
 *
 * `issue-tree.ts` passes the tree it assembles through this module, so this module imports no
 * value of that one, which Biome's `noImportCycles` would refuse: a node's life is read through
 * `issue-address.ts`'s predicates, since that module imports no value of `issue-tree.ts` or of
 * any module that does. The read's own values are read in `tree-read/tree-nodes.ts`
 * (`nodeStatesOf`), which imports no value of this module or of `issue-tree.ts`.
 */

/** What the tree read says of one node beside its kind: its life, where it has one, and whether
 *  it is reserved for its author, who is the reader. A live or settled node that every reader
 *  may see has neither. */
export interface NodeStates {
  readonly life?: NodeLife;
  readonly onlyYou?: true;
}

/** The marks `from` holds, each only where it holds one, so whatever is given them carries no
 *  key it has no value for; undefined where it holds none, so it says whether `from` carries a
 *  mark at all. */
export function marksOf(from: NodeStates | undefined): NodeStates | undefined {
  if (from?.life === undefined && from?.onlyYou === undefined) return undefined;
  return {
    ...(from.life === undefined ? {} : { life: from.life }),
    ...(from.onlyYou === undefined ? {} : { onlyYou: from.onlyYou }),
  };
}

/** `place` carrying `marks` and no other mark, whatever it carried before: `place` itself where
 *  it carries none and is given none. */
function withMarks<Place extends NodeStates>(place: Place, marks: NodeStates | undefined): Place {
  if (marks === undefined && place.life === undefined && place.onlyYou === undefined) return place;
  const { life: _life, onlyYou: _onlyYou, ...rest } = place;
  return { ...rest, ...marks } as Place;
}

/** Whether a node of `root`, or the parent line of one, carries a mark or is closed. */
function carriesMarks(root: TreeNode): boolean {
  return (
    marksOf(root) !== undefined ||
    root.closed === true ||
    marksOf(root.parentLine) !== undefined ||
    root.children.some(carriesMarks)
  );
}

/**
 * `root` with the states the tree read gives its nodes, by the read's own id: a node takes
 * the entry of its `readId`, a parent line that of its own `readId`, and a node or a line with
 * none takes no states. Each takes the marks its entry holds and no other, whatever it carried
 * before, so a tree stamped again - a view's, whose rows are the outline's nodes - carries the
 * new states alone: a live or settled node has none. A node that is overtaken is `closed`, and so
 * is every node under it in `root`. So is a node drawn under its parent's line rather than under
 * its parent's row - a view's row whose parent the view draws no row for - where that parent is
 * overtaken, or is one of `closedAbove`, the read ids of the parents that stand at or under an
 * overtaken node themselves, each with the id of the topmost such node: the node stands under
 * that parent all the same. Each closed node carries the id of the topmost overtaken node it
 * stands at or under as its `closedBy`, its own where it is that node. No other node is closed.
 * A proposed node and a closed one draw no gap, the page's own included: a gap says what
 * the issue's work still lacks, and neither is part of that work. No reader is waited on for a
 * closed node either: a later node took its place, so the page opens on none of them and draws
 * none in the waiting tone, though an open question among them is still a `question`, which `q`
 * reaches. A gap or a wait an earlier stamp took from a node is not given back. Each chip, in
 * `chips` and in `alsoUnder`, carries the marks of the node of `root` whose reference it names,
 * whatever version it pins, and a chip naming no node of `root` keeps its own, so a node is marked
 * wherever it is drawn.
 *
 * With no states and no `closedAbove`, over a tree that carries no mark, it answers `root` itself.
 */
export function stampStates(
  root: TreeNode,
  states: ReadonlyMap<string, NodeStates>,
  closedAbove?: ReadonlyMap<string, string>
): TreeNode {
  if (states.size === 0 && closedAbove === undefined && !carriesMarks(root)) return root;
  const statesOf = (readId: string | undefined): NodeStates | undefined =>
    readId === undefined ? undefined : marksOf(states.get(readId));
  // The marks of each node of `root` that has a reference, by that reference without the version
  // it pins, `undefined` for a node with none.
  const named = new Map<string, NodeStates | undefined>();
  const index = (node: TreeNode): void => {
    if (node.reference !== undefined) {
      named.set(withoutVersion(node.reference), statesOf(node.readId));
    }
    for (const child of node.children) index(child);
  };
  index(root);
  const chipWith = (chip: NodeChip): NodeChip => {
    const target = withoutVersion(chip.ref);
    return named.has(target) ? withMarks(chip, named.get(target)) : chip;
  };
  // `above` is the topmost overtaken node the parent of `node` stands at or under, if any.
  const stamp = (node: TreeNode, above: string | undefined): TreeNode => {
    const { closed: _closed, closedBy: _closedBy, ...own } = withMarks(node, statesOf(node.readId));
    const line =
      own.parentLine === undefined
        ? undefined
        : withMarks(own.parentLine, statesOf(own.parentLine.readId));
    // The topmost first: the node the parent stands under, the one above the parent line's
    // node, that node itself, and last this node.
    const top =
      above ??
      (line?.readId === undefined ? undefined : closedAbove?.get(line.readId)) ??
      (line !== undefined && isOvertaken(line) ? line.id : undefined) ??
      (isOvertaken(own) ? own.id : undefined);
    const children = own.children.map((child) => stamp(child, top));
    return {
      ...own,
      alsoUnder: own.alsoUnder.map(chipWith),
      children,
      chips: own.chips.map(chipWith),
      ...(top === undefined ? {} : { closed: true, closedBy: top, waiting: false }),
      gaps: top !== undefined || isProposed(own) ? [] : own.gaps,
      ...(line === undefined ? {} : { parentLine: line }),
    };
  };
  return stamp(root, undefined);
}

/**
 * `root`, a tree drawn from the tree read's `answer`, with the states that answer gives its nodes
 * (`nodeStatesOf`), through `stampStates`: the outline, drawn from the read with no view,
 * and a view's tree, drawn from that view's own answer, so a view's rows carry their own state.
 * A view's tree is handed the outline's as `outline`: a node or a parent line of the view whose
 * read id the view's answer holds no node for - a parent the view answers by its id, kind and
 * first line alone (`parent_line`), a node an address names that the view does not draw - takes
 * the marks the outline's node of that read id carries, which the read with no view gave it. A
 * node the view's answer holds takes that answer's marks alone, so a node the view holds live
 * stays live whatever the outline says. A row the view draws under a parent's line stands under
 * that parent as the outline holds it, since the view draws nothing above the line: closed where
 * the outline holds that parent at or under an overtaken node (`stampStates`'s `closedAbove`).
 * For a parent the view's answer holds, which the words filter drew no row of and lifted a match
 * under the line of, that is a node above the parent, whose own life is the view's: so the match
 * is closed under the words where it is closed with none. Both come from the tree read, which
 * the page has already made: no read is sent for them.
 */
export function closeOvertaken(root: TreeNode, answer: TreeAnswer, outline?: TreeNode): TreeNode {
  const states = nodeStatesOf(answer);
  if (outline === undefined) return stampStates(root, states);
  // The read id of each node and parent line of `root` that the view's answer holds no node for,
  // and of each parent line it does hold a node for.
  const held = new Set((answer.nodes ?? []).map((node) => node.id));
  const missing = new Set<string>();
  const lines = new Set<string>();
  const walk = (node: TreeNode): void => {
    if (node.readId !== undefined && !held.has(node.readId)) missing.add(node.readId);
    const line = node.parentLine?.readId;
    if (line !== undefined && held.has(line)) lines.add(line);
    else if (line !== undefined) missing.add(line);
    for (const child of node.children) walk(child);
  };
  walk(root);
  if (missing.size === 0 && lines.size === 0) return stampStates(root, states);
  // Each missing one takes the marks of the outline's last node of that read id that carries any.
  // Each line whose node the outline holds at or under an overtaken node - above it, for a node
  // the view holds - closes the rows drawn under it, under the topmost such node of the first
  // place the outline holds it closed at.
  const merged = new Map(states);
  const closedAbove = new Map<string, string>();
  const indexOutline = (node: TreeNode): void => {
    const readId = node.readId;
    if (readId !== undefined) {
      const marks = marksOf(node);
      if (marks !== undefined && missing.has(readId)) merged.set(readId, marks);
      const top = node.closed === true ? (node.closedBy ?? node.id) : undefined;
      const closes = missing.has(readId) || (lines.has(readId) && top !== node.id);
      if (top !== undefined && closes && !closedAbove.has(readId)) closedAbove.set(readId, top);
    }
    for (const child of node.children) indexOutline(child);
  };
  indexOutline(outline);
  return stampStates(root, merged, closedAbove);
}

/** The marks of each node of `root` that has a reference and a mark, by that reference without
 *  the version it pins (`tree-keys.ts`'s `withoutVersion`): what a place naming a node by its
 *  address draws beside it. An address that pins a version is read through `marksAt`,
 *  which takes the version off as the map's keys have it off. */
export function statesByReference(root: TreeNode): ReadonlyMap<string, NodeStates> {
  const found = new Map<string, NodeStates>();
  const walk = (node: TreeNode): void => {
    const marks = marksOf(node);
    if (node.reference !== undefined && marks !== undefined) {
      found.set(withoutVersion(node.reference), marks);
    }
    for (const child of node.children) walk(child);
  };
  walk(root);
  return found;
}

/** The marks `states` (`statesByReference`) holds for the node `reference` names, whatever
 *  version the reference pins; undefined where it holds none. */
export function marksAt(
  states: ReadonlyMap<string, NodeStates>,
  reference: string
): NodeStates | undefined {
  return states.get(withoutVersion(reference));
}
