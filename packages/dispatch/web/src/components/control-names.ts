/**
 * The one spelling of Dispatch's search, sort and filter vocabulary. Each control writes its name
 * into `data-control`, so a browser row, a page and a reviewer all name the same thing the same
 * way, and a second control built by hand anywhere under `web/src` is a thing a check can find.
 * The values are the attribute's, not the code's: renaming one renames every selector that reads
 * it, so they are fixed.
 */
export const CONTROL = Object.freeze({
  /** An action of the control bar (`BarAction`): a quiet button, or the page's one main action
   *  drawn filled. */
  barAction: "bar-action",
  chip: "chip",
  columnFilter: "column-filter",
  columnsMenu: "columns-menu",
  controlRow: "control-row",
  dataTable: "data-table",
  displayMenu: "display-menu",
  /** The control bar's **More** (⋯) menu (`MoreMenu`). */
  moreMenu: "more-menu",
  pager: "pager",
  panelTrigger: "panel-trigger",
  searchBox: "search-box",
  /** The switch after the control bar's search box (`SegmentedControl`). */
  segmentedControl: "segmented-control",
  selectionBar: "selection-bar",
  tableShell: "table-shell",
  /** The trigger of a page's own panel that narrows nothing: the Issues tab's Save view and its
   *  (i) hint, so the row keeps one `panel-trigger`, the one over the narrowings. */
  viewPanel: "view-panel",
} as const);

/** What a `data-control` attribute carries: no page invents a sixteenth. */
export type ControlName = (typeof CONTROL)[keyof typeof CONTROL];

/** The control `root` holds under the name `name` writes into its `data-control`, or null. */
export function controlIn(root: ParentNode, name: ControlName): HTMLElement | null {
  return root.querySelector<HTMLElement>(`[data-control="${name}"]`);
}

/** The words a screen reader names `node` by: its `aria-label`, else the words it draws, with
 *  what it hides from a screen reader (`aria-hidden`, such as a bar action's glyph) left out. */
function spokenName(node: Node): string {
  if (node instanceof Element) {
    if (node.getAttribute("aria-hidden") === "true") return "";
    const label = node.getAttribute("aria-label");
    if (label !== null) return label;
  }
  if (node.nodeType === Node.TEXT_NODE) return node.textContent ?? "";
  return [...node.childNodes].map(spokenName).join("");
}

/** The control `root` holds under the name `name` writes into its `data-control` whose
 *  accessible name is `label`, or null: the Artifacts tab's **Upload**, one of its bar's actions,
 *  found by the label `BarAction` names it by rather than by its place among them. */
export function namedControlIn(
  root: ParentNode,
  name: ControlName,
  label: string
): HTMLElement | null {
  const named = root.querySelectorAll<HTMLElement>(`[data-control="${name}"]`);
  return [...named].find((control) => spokenName(control).trim() === label) ?? null;
}

/** The control bar's own width under which it takes its narrow layout (`ControlRow`). One width,
 *  so that each width has one layout a check can read; a browser row reads the bar against it. */
export const NARROW_BELOW = 640;

/** How much room a control takes in its row. `compact` is for a toolbar that holds more controls
 *  than a phone's width fits at the comfortable size: the Delivery page's, and a table's own
 *  header filters, pager and selection bar. `toolbar` is the compact size of a table toolbar whose
 *  controls stand at one height from 1280 px up: the project List's control row and the
 *  priorities pages' toolbars (dispatch://INFRA-2947, R15). */
export type ControlSize = "comfortable" | "compact" | "toolbar";

/** The 34 px height a table toolbar's controls share from 1280 px up (dispatch://INFRA-2947, R15);
 *  `xl:min-h-0` lifts each control's 44 px phone minimum there. */
export const toolbarHeight = "xl:h-8.5 xl:min-h-0";

/** The side padding of a control's words at each size: `controlPadding`'s, and what a box sized
 *  to a control's words takes with none of its height (`ControlRow`'s placeholder sizer). */
export const controlSidePadding: Record<ControlSize, string> = {
  comfortable: "px-3",
  compact: "px-2",
  toolbar: "px-2",
};

/** 44 px: the touch target of the comfortable size. The compact size keeps that target wherever
 *  a finger is the pointer and gives it up only from `md`, where a mouse is. The toolbar size
 *  keeps the 44 px floor `styles.css` gives every control below 1280 px, and from 1280 px up
 *  stands at one height, 34 px (`toolbarHeight`), whatever its control holds. The font is each
 *  control's own: a chip reads at `text-xs` and a search box at `text-sm`, and a size that
 *  carried one would change every chip in the application. */
export const controlPadding: Record<ControlSize, string> = {
  comfortable: `min-h-11 ${controlSidePadding.comfortable} py-2`,
  compact: `min-h-11 ${controlSidePadding.compact} py-1 md:min-h-8`,
  toolbar: `min-h-11 ${controlSidePadding.toolbar} py-1 ${toolbarHeight}`,
};

/** A glyph's touch target at each size, a chip's link and a table column's funnel: 44 px each
 *  way, which the compact size keeps wherever a finger is the pointer and gives up only from
 *  `md`, where a mouse is, for 32 px. */
export const glyphTarget: Record<ControlSize, string> = {
  comfortable: "min-h-11 min-w-11",
  compact: "min-h-11 min-w-11 md:min-h-8 md:min-w-8",
  // A header's funnel is no toolbar's control: at the toolbar size it takes the compact target.
  toolbar: "min-h-11 min-w-11 md:min-h-8 md:min-w-8",
};
