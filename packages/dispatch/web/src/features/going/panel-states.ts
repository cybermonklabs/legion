/**
 * Where the command bar's conversation is on the screen, and what each key, press and move of the
 * person does to it: the state table `panel-states.test.ts` holds row by row. It is closed, over
 * the page (the bar, a dialog the page sits dimmed behind), or at the side (a panel at the right
 * that leaves the page usable). Nothing here moves anything; the shell reads the answer.
 */

/** Where the conversation is: nowhere, over the page, or at the side. */
export type PanelPlace = "closed" | "over" | "side";

/** What the person does: Ctrl-K or Cmd-K, Ctrl-Shift-K or Cmd-Shift-K, the bar's "Keep at the
 *  side" button, Esc in a box with words in it, Esc in an empty box (`working`: whether the
 *  conversation's run works then), a navigation to another page, or a click on the page. */
export type PanelEvent =
  | { readonly kind: "mod_k" }
  | { readonly kind: "mod_shift_k" }
  | { readonly kind: "keep_at_side" }
  | { readonly kind: "esc_full" }
  | { readonly kind: "esc_empty"; readonly working: boolean }
  | { readonly kind: "navigation" }
  | { readonly kind: "page_click" };

/** A transition: the next place, whether the conversation's run is to stop, and whether the item
 *  the click was on becomes the context of the next request. */
export interface PanelTransition {
  readonly place: PanelPlace;
  readonly stopRun?: true;
  readonly context?: true;
}

/** How the bar keeps the conversation at the side: by `$mod+Shift+k`, as everywhere, or - in a
 *  browser whose page never gets Ctrl-Shift-K, Firefox's own key for its Web Console - by Ctrl-K
 *  pressed while the bar is open (`sideByModK`). The button "Keep at the side" stays in both. */
export interface PanelKeys {
  readonly sideByModK: boolean;
}

/** The keys of a browser that gets Ctrl-Shift-K. */
export const PANEL_KEYS: PanelKeys = { sideByModK: false };

/** The keys of this browser: Firefox keeps Ctrl-Shift-K for its Web Console, so there Ctrl-K in
 *  the open bar keeps the conversation at the side, and the button stays. */
export const BROWSER_PANEL_KEYS: PanelKeys = {
  sideByModK: typeof navigator !== "undefined" && /Firefox\//.test(navigator.userAgent),
};

/**
 * The next place of the conversation in `place` on `event`. Ctrl-K opens the bar over the page and
 * closes it again; from the side it brings the conversation back over the page. Ctrl-Shift-K and
 * the button keep it at the side, and Ctrl-Shift-K at the side closes it. Esc closes the bar, but
 * Esc in its empty box while the run works stops the run and leaves the bar open. The panel at the
 * side stays on every navigation and every click on the page, and a click there names the item
 * the next request is about; over the page a navigation closes the bar, and so does a click on
 * the page behind it.
 */
export function panelNext(
  place: PanelPlace,
  event: PanelEvent,
  keys: PanelKeys = PANEL_KEYS
): PanelTransition {
  switch (place) {
    case "closed":
      switch (event.kind) {
        case "mod_k":
          return { place: "over" };
        case "mod_shift_k":
          return { place: "side" };
        default:
          return { place };
      }
    case "over":
      switch (event.kind) {
        case "mod_k":
          return { place: keys.sideByModK ? "side" : "closed" };
        case "mod_shift_k":
        case "keep_at_side":
          return { place: "side" };
        case "esc_full":
          return { place: "closed" };
        case "esc_empty":
          return event.working ? { place, stopRun: true } : { place: "closed" };
        case "navigation":
        case "page_click":
          return { place: "closed" };
      }
      break;
    case "side":
      switch (event.kind) {
        case "mod_k":
          return { place: "over" };
        case "mod_shift_k":
          return { place: "closed" };
        case "esc_empty":
          return event.working ? { place, stopRun: true } : { place };
        case "page_click":
          return { context: true, place };
        default:
          return { place };
      }
  }
  return { place };
}
