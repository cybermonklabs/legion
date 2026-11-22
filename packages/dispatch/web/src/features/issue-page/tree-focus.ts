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
