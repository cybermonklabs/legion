import { focusOnDocument } from "../shell/roving";

/**
 * What the page's trees share about where the focus is, kept apart from the stage and the
 * outline so a view's body drawn inside the stage can import it: the stage imports each view,
 * so a view that imported the stage would close a cycle of imports.
 */

/** The row of a slot's tree that holds its one tab stop (the WAI-ARIA tree pattern). */
export const TAB_STOP_ROW = '[role="tree"] [role="treeitem"][tabindex="0"]';

/** The outline's row whose key is `rowKey` (`tree-keys.ts`'s `rowKeyOf`), as a selector. */
export function rowSelectorOf(rowKey: string): string {
  return `[data-node-row][data-row-key="${CSS.escape(rowKey)}"]`;
}

/** Every row of the outline that draws the node `id`, as a selector: a node has one row, or a
 *  row for each place it is drawn at. */
export function nodeRowSelectorOf(id: string): string {
  return `[data-node-row][data-node-id="${CSS.escape(id)}"]`;
}

/** Whether nothing holds the focus that the tree is to keep from it: it is on nothing, on the
 *  page itself, or on an element that holds `tree` - the app's main element, which the app
 *  focuses at each page change but the first (`app.tsx`), so a page opened by a link starts with
 *  the focus there, and no reader put it there. */
export function focusIsLoose(tree: HTMLElement | null): boolean {
  return focusOnDocument() || (tree !== null && document.activeElement?.contains(tree) === true);
}

/** The row of `tree` that holds the focus, on the row itself or on a control inside it; null
 *  where the focus is on no row of it. */
export function focusedRowIn(tree: HTMLElement | null): HTMLElement | null {
  const row = document.activeElement?.closest<HTMLElement>("[data-node-row]") ?? null;
  return row !== null && tree?.contains(row) === true ? row : null;
}

/** The row that holds the focus as a render of a tree is drawn: its key (`tree-keys.ts`'s
 *  `rowKeyOf`), its place among the rows' keys in the order they are drawn, and its node. The
 *  outline and the Spec view each note it at every render, so a later render that drops the row
 *  can still tell where it stood (`IssueTree.tsx`'s and `SpecFilterBody.tsx`'s `heldRow`). */
export interface FocusedRowNote {
  readonly index: number;
  readonly key: string;
  readonly node: string | undefined;
}

/** The note of the row of `tree` that holds the focus (`focusedRowIn`), placed among the rows
 *  whose keys `order` holds in the order they are drawn; undefined where the focus is on no row
 *  of it or on a row with no key. */
export function focusedRowNote(
  tree: HTMLElement | null,
  order: readonly string[]
): FocusedRowNote | undefined {
  const row = focusedRowIn(tree);
  const key = row?.dataset.rowKey;
  return key === undefined
    ? undefined
    : { index: order.indexOf(key), key, node: row?.dataset.nodeId };
}

/** The row of `tree` nearest to the place `index` among the rows whose keys `order` holds in the
 *  order they are drawn: the row now at that place, or the last where none is; null where `tree`
 *  draws neither. A tree hands the focus on to it where a drop the reader caused takes out the
 *  row that held the focus (`IssueTree.tsx`'s and `SpecFilterBody.tsx`'s `heldRow`). */
export function nearestRowIn(
  tree: HTMLElement | null,
  order: readonly string[],
  index: number
): HTMLElement | null {
  const key = order[Math.min(index, order.length - 1)];
  return key === undefined ? null : (tree?.querySelector<HTMLElement>(rowSelectorOf(key)) ?? null);
}
