import type { TreeNode } from "./issue-tree";

/**
 * How much of the issue's tree is drawn. Pure, over the model `issue-tree.ts` builds, so the
 * rows, the zoom and the unit tests fold the same way.
 *
 * Every node is drawn, unfolded and whole, at every depth: the page folds no node itself (R11).
 * Two things leave a node's children out, and nothing else does:
 *
 * - **The reader's fold.** A row the reader folded, by its fold control, the left arrow or `o`,
 *   draws its node's first line (`node-line.tsx`'s `FirstLine`) and how many nodes it holds
 *   (`TreeRow.folded`), until they unfold it, and the fold lasts for the load
 *   (`OpenedBranches`' `closed`). A fold is the row's and not the node's: a node drawn under two
 *   parents is two rows, and the reader folds each on its own (`placeOf`).
 * - **A node's page.** A node draws its first `CHILD_PAGE` children, in the read's order, and
 *   after them one row that says how many are left and draws the next `CHILD_PAGE` under that
 *   row alone (`PageRow`), so no list the outline draws grows without bound (R15). A page is no
 *   fold: every row it draws is whole. A child that something names - the focus, an address, a
 *   write - is drawn with every child before it (`treeRows`' `named` and `placed`), so a new row
 *   past the first page lands in view.
 *
 * Every level is drawn. Past `INDENT_LEVELS` levels a row's indent stops growing (`indentOf`), so
 * a node's words keep their width at 390 px.
 */

/** How many of a node's children one page draws (R15). */
export const CHILD_PAGE = 50;

/** How many levels a row's indent grows through: a row deeper than this stands at the indent of
 *  this level (R11, root R16's six levels). */
const INDENT_LEVELS = 6;

/** What the reader has done to the outline by hand: the rows they folded, and the pages of
 *  children they drew. Both are kept by the row's place (`placeOf`), so what the reader does to
 *  one row of a node drawn under several parents is done to that row alone. */
export interface OpenedBranches {
  /** The places of the rows whose children the reader folded. */
  readonly closed: ReadonlySet<string>;
  /** How many of its children each row draws where the reader drew more than its first page,
   *  by the row's place (`withNextPage`). */
  readonly pages: ReadonlyMap<string, number>;
}

/** What joins the ids of a place, from the issue down. */
const PLACE_SEPARATOR = ">";

/** Where a row stands: the path from the issue down to its node, `trail` and then the node's
 *  own id. It tells the rows of one node drawn under two parents apart, and every row below
 *  each of them, and it is the key the page knows each row by (`rowKeyOf`). */
export function placeOf(trail: readonly string[], id: string): string {
  return [...trail, id].join(PLACE_SEPARATOR);
}

/** The place of the row of node `id` drawn under the row at `place`: the one rule by which every
 *  walk down the tree extends a place, so it gives the string `placeOf` gives that row. */
function childPlace(place: string, id: string): string {
  return `${place}${PLACE_SEPARATOR}${id}`;
}

/** The place of each node of `path`, a chain of node ids from the issue down, the outermost
 *  first. */
export function placesAlong(path: readonly string[]): string[] {
  let place: string | undefined;
  return path.map((id) => {
    place = place === undefined ? id : childPlace(place, id);
    return place;
  });
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
 * at is drawn open by definition; every row below it is unfolded unless the reader folded it.
 *
 * `above` is the path down to `root` itself, outermost first: the ids a reader who zoomed into
 * it left above the tree (`IssueTree.tsx`). It seeds the trails, so every row is keyed by its
 * path from the issue however much of the tree is drawn, and the folds, the pages, the editing
 * and the tree's one tab stop a key holds survive a zoom in both directions (`placeOf`).
 *
 * `named` are the nodes an address or a write names by their id alone: each is drawn with every
 * child before it, down the path to it from `root` that `pathTo` finds, past its parent's page
 * where it stands there. `placed` are the rows the page names by their place - the row the tab
 * stop rests on - each drawn so at that place alone.
 */
export function treeRows(
  root: TreeNode,
  opened: OpenedBranches = NOTHING_OPENED,
  above: readonly string[] = [],
  named: readonly string[] = [],
  placed: readonly string[] = []
): DrawnRow[] {
  const start = placeOf(above, root.id);
  const reached = reachedChildren(root, start, named, placed);
  const rows: DrawnRow[] = [];
  const walk = (node: TreeNode, level: number, trail: readonly string[], place: string): void => {
    const holds = node.children.length > 0;
    const unfolded = level === 1 || !opened.closed.has(place);
    rows.push({
      ...(holds ? { expanded: unfolded } : {}),
      ...(holds && !unfolded ? { folded: node.held } : {}),
      kind: "node",
      level,
      node,
      trail,
    });
    if (!holds || !unfolded) return;
    const drawn = Math.max(opened.pages.get(place) ?? CHILD_PAGE, reached.get(place) ?? 0);
    const below = [...trail, node.id];
    for (const child of node.children.slice(0, drawn)) {
      walk(child, level + 1, below, childPlace(place, child.id));
    }
    const left = node.children.length - drawn;
    if (left > 0) rows.push({ kind: "page", left, level: level + 1, of: node, trail });
  };
  walk(root, 1, above, start);
  return rows;
}

/** How many children each row on the path down to a named row draws at least, by the row's
 *  place: every child up to the one the path goes through. A node of `named`, named by its id
 *  alone, is found where `pathTo` finds it, its first placement in reading order; a row of
 *  `placed` is found at its own place, so a row named under one of a node's placements draws
 *  nothing more under another. `start` is the place of `root`. */
function reachedChildren(
  root: TreeNode,
  start: string,
  named: readonly string[],
  placed: readonly string[]
): ReadonlyMap<string, number> {
  const reached = new Map<string, number>();
  const ids = new Set(named);
  const places = new Set(placed);
  // A child is walked into while an id is still looked for anywhere, or where a place still
  // looked for is the child's own or one below it.
  const leadsOn = (place: string): boolean => {
    if (ids.size > 0) return true;
    const prefix = childPlace(place, "");
    for (const one of places) if (one === place || one.startsWith(prefix)) return true;
    return false;
  };
  // Preorder finds the same first placement of each named id as pathTo.
  const walk = (node: TreeNode, place: string): boolean => {
    const byId = ids.delete(node.id);
    let found = places.delete(place) || byId;
    for (let at = 0; at < node.children.length && (ids.size > 0 || places.size > 0); at += 1) {
      const child = node.children[at];
      if (child === undefined) continue;
      const below = childPlace(place, child.id);
      if (leadsOn(below) && walk(child, below)) {
        reached.set(place, Math.max(reached.get(place) ?? 0, at + 1));
        found = true;
      }
    }
    return found;
  };
  if (ids.size > 0 || places.size > 0) walk(root, start);
  return reached;
}

/** `opened` with `page`'s row drawing the next `CHILD_PAGE` of its node's children: what a press
 *  on the row that pages them does, under that row alone. */
export function withNextPage(opened: OpenedBranches, page: PageRow): OpenedBranches {
  const pages = new Map(opened.pages);
  pages.set(placeOf(page.trail, page.of.id), page.of.children.length - page.left + CHILD_PAGE);
  return { closed: opened.closed, pages };
}

/** `opened` with the row at `place` folded, or unfolded where `open` holds: what its fold
 *  control, the left and right arrows and `o` do (R11). Every other row of its node stays as it
 *  was. */
export function withFold(opened: OpenedBranches, place: string, open: boolean): OpenedBranches {
  if (opened.closed.has(place) !== open) return opened;
  const closed = new Set(opened.closed);
  if (open) closed.delete(place);
  else closed.add(place);
  return { closed, pages: opened.pages };
}

/** `opened` with each row of `places` unfolded and the pages kept, and `opened` itself where the
 *  reader folded none of them: what a path drawn open to send the reader to a row does. */
export function withUnfolded(opened: OpenedBranches, places: readonly string[]): OpenedBranches {
  if (!places.some((place) => opened.closed.has(place))) return opened;
  return {
    closed: new Set([...opened.closed].filter((place) => !places.includes(place))),
    pages: opened.pages,
  };
}

/** `opened` with every row the reader folded unfolded, and the pages of children they drew
 *  kept: what `z` does (R11). */
export function withEveryFoldUndone(opened: OpenedBranches): OpenedBranches {
  return opened.closed.size === 0 ? opened : { closed: new Set(), pages: opened.pages };
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
