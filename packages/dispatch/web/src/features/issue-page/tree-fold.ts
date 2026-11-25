import { drawnLine, firstLineOf, lineWords, type TreeNode } from "./issue-tree";

/**
 * How much of the issue's tree is drawn. Pure, over the model `issue-tree.ts` builds, so the
 * rows, the zoom and the unit tests fold the same way.
 *
 * Every node is drawn, unfolded and whole, at every depth: the page folds no node itself (R11).
 * Two things leave a node's children out, and nothing else does:
 *
 * - **The reader's fold.** A node the reader folded, by its fold control, the left arrow or `o`,
 *   draws its first line and how many nodes it holds (`foldedLine`, `TreeRow.folded`), until
 *   they unfold it, and the fold lasts for the load (`OpenedBranches`' `closed`).
 * - **A node's page.** A node draws its first `CHILD_PAGE` children, in the read's order, and
 *   after them one row that says how many are left and draws the next `CHILD_PAGE`
 *   (`PageRow`), so no list the outline draws grows without bound (R15). A page is no fold:
 *   every row it draws is whole. A child that something names - the focus, an address, a write -
 *   is drawn with every child before it (`treeRows`' `named`), so a new row past the first page
 *   lands in view.
 *
 * Every level is drawn. Past `INDENT_LEVELS` levels a row's indent stops growing (`indentOf`), so
 * a node's words keep their width at 390 px.
 */

/** How many of a node's children one page draws (R15). */
export const CHILD_PAGE = 50;

/** How many levels a row's indent grows through: a row deeper than this stands at the indent of
 *  this level (R11, root R16's six levels). */
export const INDENT_LEVELS = 6;

/** What the reader has done to the outline by hand: the nodes they folded, and the pages of
 *  children they drew. */
export interface OpenedBranches {
  /** The nodes whose children the reader folded. */
  readonly closed: ReadonlySet<string>;
  /** How many of its children each node draws where the reader drew more than its first page,
   *  by the node's id (`withNextPage`). */
  readonly pages: ReadonlyMap<string, number>;
}

/** One row of the tree as the fold leaves it: the node, how deep it sits, and what its own
 *  twisty says. */
export interface TreeRow {
  /** `true` when its children are drawn, `false` when it holds children the reader folded, and
   *  undefined for a node that holds none. */
  readonly expanded?: boolean;
  /** How many nodes the row holds while the reader has it folded: every node under it that the
   *  page holds. Undefined for a row that is not folded. */
  readonly folded?: number;
  readonly level: number;
  readonly node: TreeNode;
  /** The ids of the nodes this row sits under, the outermost first and its own parent last;
   *  empty for the root. One node is drawn under two parents (R23), so this is what tells its
   *  rows apart, down to the rows below them (`rowKeyOf`). */
  readonly trail: readonly string[];
}

/** The one row after a node's drawn children that says how many are left and draws the next
 *  page of them (R15). */
export interface PageRow {
  /** How many of the node's children no row draws yet. */
  readonly left: number;
  /** The level its node's children stand at. */
  readonly level: number;
  /** The node whose children it pages. */
  readonly of: TreeNode;
  /** The ids of the nodes that node sits under, as its own `TreeRow`'s trail. */
  readonly trail: readonly string[];
}

/** Every row the tree draws, in reading order: a node's own row, or the row that pages a node's
 *  children. */
export type DrawnRow =
  | ({ readonly kind: "node" } & TreeRow)
  | ({ readonly kind: "page" } & PageRow);

/** Nothing folded and no page drawn by hand, which is how the page opens. */
export const NOTHING_OPENED: OpenedBranches = { closed: new Set(), pages: new Map() };

/**
 * Every row the tree draws over `root`, in reading order. The root is always unfolded, because
 * an issue drawn as one folded row would say nothing at all, and the node a zoom roots the tree
 * at is drawn open by definition; every node below it is unfolded unless the reader folded it.
 *
 * `above` is the path down to `root` itself, outermost first: the ids a reader who zoomed into
 * it left above the tree (`IssueTree.tsx`). It seeds the trails, so every row is keyed by its
 * path from the issue however much of the tree is drawn, and the editing and the tree's one tab
 * stop a key holds survive a zoom in both directions (`rowKeyOf`).
 *
 * `named` are the nodes the focus, an address or a write names: each is drawn with every child
 * before it, down the path to it from `root` (`pathTo`), past its parent's page where it stands
 * there.
 */
export function treeRows(
  root: TreeNode,
  opened: OpenedBranches = NOTHING_OPENED,
  above: readonly string[] = [],
  named: readonly string[] = []
): DrawnRow[] {
  const reached = reachedChildren(root, named);
  const rows: DrawnRow[] = [];
  const walk = (node: TreeNode, level: number, trail: readonly string[]): void => {
    const holds = node.children.length > 0;
    const unfolded = level === 1 || !opened.closed.has(node.id);
    rows.push({
      ...(holds ? { expanded: unfolded } : {}),
      ...(holds && !unfolded ? { folded: node.held } : {}),
      kind: "node",
      level,
      node,
      trail,
    });
    if (!holds || !unfolded) return;
    const drawn = Math.max(opened.pages.get(node.id) ?? CHILD_PAGE, reached.get(node.id) ?? 0);
    const below = [...trail, node.id];
    for (const child of node.children.slice(0, drawn)) walk(child, level + 1, below);
    const left = node.children.length - drawn;
    if (left > 0) rows.push({ kind: "page", left, level: level + 1, of: node, trail });
  };
  walk(root, 1, above);
  return rows;
}

/** How many children each node on the path down to a node of `named` draws at least, by the
 *  node's id: every child up to the one the path goes through. */
function reachedChildren(root: TreeNode, named: readonly string[]): ReadonlyMap<string, number> {
  const reached = new Map<string, number>();
  for (const id of named) {
    const path = pathTo(root, id) ?? [];
    for (let at = 1; at < path.length; at += 1) {
      const parent = path[at - 1];
      const child = path[at];
      if (parent === undefined || child === undefined) continue;
      const upTo = parent.children.indexOf(child) + 1;
      reached.set(parent.id, Math.max(reached.get(parent.id) ?? 0, upTo));
    }
  }
  return reached;
}

/** `opened` with `page`'s node drawing the next `CHILD_PAGE` of its children: what a press on
 *  the row that pages them does. */
export function withNextPage(opened: OpenedBranches, page: PageRow): OpenedBranches {
  const pages = new Map(opened.pages);
  pages.set(page.of.id, page.of.children.length - page.left + CHILD_PAGE);
  return { closed: opened.closed, pages };
}

/** `opened` with the node `id` folded, or unfolded where `open` holds: what its fold control,
 *  the left and right arrows and `o` do (R11). */
export function withFold(opened: OpenedBranches, id: string, open: boolean): OpenedBranches {
  if (opened.closed.has(id) !== open) return opened;
  const closed = new Set(opened.closed);
  if (open) closed.delete(id);
  else closed.add(id);
  return { closed, pages: opened.pages };
}

/** `opened` with every node the reader folded unfolded, and the pages of children they drew
 *  kept: what `z` does (R11). */
export function withEveryFoldUndone(opened: OpenedBranches): OpenedBranches {
  return opened.closed.size === 0 ? opened : { closed: new Set(), pages: opened.pages };
}

/** What a folded node's row draws of its words: its text to its first sentence end, or to the
 *  first line's limit, cut at a word (R11's first-line rule, `firstLineOf`), cut from the words
 *  every one-line drawing of a node is cut from (`lineWords`). */
export function foldedLine(node: Pick<TreeNode, "text" | "tree">): string {
  return drawnLine(firstLineOf(lineWords(node)));
}

/** How many levels a row at `level` is indented, the tree's first row at none: one a level up
 *  to `INDENT_LEVELS`, and no more past it. */
export function indentOf(level: number): number {
  return Math.min(level, INDENT_LEVELS) - 1;
}

/** The nodes above the one the outline is rooted at, outermost first: the breadcrumb a reader
 *  zooms back out through. Empty while the tree is on the issue itself. */
export function zoomBreadcrumb(root: TreeNode, zoom: string | undefined): TreeNode[] {
  return (pathTo(root, zoom ?? root.id) ?? [root]).slice(0, -1);
}

/** The chain from the root down to `id`, the root first; undefined when the tree holds no such
 *  node. */
export function pathTo(root: TreeNode, id: string): TreeNode[] | undefined {
  if (root.id === id) return [root];
  for (const child of root.children) {
    const below = pathTo(child, id);
    if (below !== undefined) return [root, ...below];
  }
  return undefined;
}
