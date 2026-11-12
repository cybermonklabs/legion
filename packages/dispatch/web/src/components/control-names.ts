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

/** How much room a control takes in its row. `compact` exists for the project List, whose toolbar
 *  holds more controls than a phone's width fits at the comfortable size. */
export type ControlSize = "comfortable" | "compact";

/** 44 px: the touch target every page but the project List draws. The compact size keeps that
 *  target wherever a finger is the pointer and gives it up only from `md`, where a mouse is.
 *  The font is each control's own: a chip reads at `text-xs` and a search box at `text-sm`, and
 *  a size that carried one would change every chip in the application. */
export const controlPadding: Record<ControlSize, string> = {
  comfortable: "min-h-11 px-3 py-2",
  compact: "min-h-11 px-2 py-1 md:min-h-8",
};
