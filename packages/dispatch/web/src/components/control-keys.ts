import type { RefObject } from "react";

import { useKeymap } from "../features/shell/keymap";
import type { KeymapActionId } from "../features/shell/keymap-catalogue";
import { noteOpenedFrom } from "./AnchoredPanel";
import { CONTROL, type ControlName, controlIn } from "./control-names";
import { focusableIn, panelOf } from "./focus-place";

/**
 * The keys of a page's control row (R24): Shift+F opens its filter panel and Shift+V its display
 * menu, so a reader on the page reaches either without walking the row by Tab. Each is an action
 * of the catalogue in the scope of the page that draws the row, which `ControlRow` takes as its
 * `keyScope`.
 */

/** The scopes a control row answers its keys in. `dialog` is the Inbox's drawer, the one dialog
 *  that draws a control row: it registers the Inbox's actions, under the Inbox's keys, as the
 *  drawer's other bindings do (`useKeymap`). */
export type ControlRowScope = "agents" | "board" | "delivery" | "dialog" | "inbox" | "project";

/** The two actions of each scope, by the control each presses. A page that draws no display
 *  menu names no display action, and its Shift+V stays free. */
const ROW_ACTIONS: Record<
  ControlRowScope,
  { readonly display?: KeymapActionId; readonly filter: KeymapActionId }
> = {
  agents: { filter: "agents.filter" },
  board: { filter: "board.filter" },
  delivery: { filter: "delivery.filter" },
  dialog: { display: "inbox.display", filter: "inbox.filter" },
  inbox: { display: "inbox.display", filter: "inbox.filter" },
  project: { display: "project.display", filter: "project.filter" },
};

/**
 * Opens the panel of `row`'s control `name` - its filter trigger (`CONTROL.panelTrigger`) or its
 * display menu (`CONTROL.displayMenu`) - as a press on that control does, so the focus goes to
 * the panel's first control. A panel that already stands open, as the Board's does when the
 * reader's stored choice opened it, stays open: the focus goes to its first control, and the key
 * closes nothing. Either way the panel first notes where the focus is (`noteOpenedFrom`), so its
 * Escape hands the focus back there, to the page itself when nothing was focused; Tab out of the
 * panel and a choice in it still leave the reader on the trigger. Answers whether `row` draws
 * that control.
 */
export function openRowControl(row: Element, name: ControlName): boolean {
  const trigger = controlIn(row, name);
  if (trigger === null) return false;
  noteOpenedFrom(trigger);
  if (trigger.getAttribute("aria-expanded") !== "true") {
    trigger.click();
    return true;
  }
  const panel = panelOf(trigger);
  if (panel !== null) (focusableIn(panel)[0] ?? panel).focus();
  return true;
}

/**
 * Registers Shift+F and Shift+V for the control row `rowRef` holds, in `scope`, the scope of the
 * page that draws the row; with no scope it registers nothing, for a row whose page has no such
 * keys (the command bar's). Neither key fires while the reader types or works a panel (no
 * `inEditable`), and each applies only while the row draws its control, so the shortcut help
 * reads a key that would do nothing as not available.
 */
export function useControlRowKeys(
  scope: ControlRowScope | undefined,
  rowRef: RefObject<HTMLElement | null>
): void {
  const actions = scope === undefined ? undefined : ROW_ACTIONS[scope];
  const controls = [
    [actions?.filter, CONTROL.panelTrigger],
    [actions?.display, CONTROL.displayMenu],
  ] as const;
  // With no scope nothing is registered, so the scope it would go under is never read.
  useKeymap(
    scope ?? "global",
    controls.flatMap(([action, name]) =>
      action === undefined
        ? []
        : [
            {
              action,
              run: () => {
                if (rowRef.current !== null) openRowControl(rowRef.current, name);
              },
              when: () => rowRef.current !== null && controlIn(rowRef.current, name) !== null,
            },
          ]
    )
  );
}
