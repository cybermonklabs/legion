/**
 * The one spelling of Dispatch's search, sort and filter vocabulary. Each control writes its name
 * into `data-control`, so a browser row, a page and a reviewer all name the same thing the same
 * way, and a second control built by hand anywhere under `web/src` is a thing a check can find.
 * The values are the attribute's, not the code's: renaming one renames every selector that reads
 * it, so they are fixed.
 */
export const CONTROL = Object.freeze({
  chip: "chip",
  columnFilter: "column-filter",
  columnsMenu: "columns-menu",
  controlRow: "control-row",
  dataTable: "data-table",
  displayMenu: "display-menu",
  pager: "pager",
  panelTrigger: "panel-trigger",
  searchBox: "search-box",
  selectionBar: "selection-bar",
  tableShell: "table-shell",
} as const);

/** What a `data-control` attribute carries: no page invents a twelfth. */
export type ControlName = (typeof CONTROL)[keyof typeof CONTROL];

/** The control `root` holds under the name `name` writes into its `data-control`, or null. */
export function controlIn(root: ParentNode, name: ControlName): HTMLElement | null {
  return root.querySelector<HTMLElement>(`[data-control="${name}"]`);
}

/** How much room a control takes in its row. `compact` is for a toolbar that holds more controls
 *  than a phone's width fits at the comfortable size: the Delivery page's, a table's own header
 *  filters, pager and selection bar, and the chips inside the project List's filter panel
 *  (`features/project/issues-table-page.tsx`). `toolbar` is the compact size of a table toolbar
 *  whose controls stand at one height from 1280 px up: the project List's control row and the
 *  priorities pages' toolbars (dispatch://INFRA-2947, R15). */
export type ControlSize = "comfortable" | "compact" | "toolbar";

/** The 34 px height a table toolbar's controls share from 1280 px up (dispatch://INFRA-2947, R15);
 *  `xl:min-h-0` lifts each control's 44 px phone minimum there. */
export const toolbarHeight = "xl:h-8.5 xl:min-h-0";

/** 44 px: the touch target of the comfortable size. The compact size keeps that target wherever
 *  a finger is the pointer and gives it up only from `md`, where a mouse is. The toolbar size
 *  keeps the 44 px floor `styles.css` gives every control below 1280 px, and from 1280 px up
 *  stands at one height, 34 px (`toolbarHeight`), whatever its control holds. The font is each
 *  control's own: a chip reads at `text-xs` and a search box at `text-sm`, and a size that
 *  carried one would change every chip in the application. */
export const controlPadding: Record<ControlSize, string> = {
  comfortable: "min-h-11 px-3 py-2",
  compact: "min-h-11 px-2 py-1 md:min-h-8",
  toolbar: `min-h-11 px-2 py-1 ${toolbarHeight}`,
};
