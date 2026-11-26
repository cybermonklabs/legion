import { type RefObject, useEffect } from "react";

import { inputMethodKey } from "../lib/input-method-key";

export interface PanelDismissOptions {
  /** Closing the panel, which is the host's state: the hook never holds `open` of its own. */
  readonly onOpenChange: (open: boolean) => void;
  readonly open: boolean;
  /** The panel itself; a pointer press inside it is not an outside press. */
  readonly panelRef: RefObject<HTMLElement | null>;
  /** The control that opens the panel: a press on it is not an outside press either, and
   *  Escape puts the focus back on it. */
  readonly triggerRef: RefObject<HTMLElement | null>;
}

/** The mark of a host the person cannot see: a live view that lingers behind the page in front
 *  (`MountedLiveViews`), which is still drawn, and keeps what was open in it. */
const HIDDEN_HOST = '[aria-hidden="true"]';

/**
 * How a small popover panel - the context ring's and the message box's question mark - is
 * dismissed: Escape closes it wherever the focus is and puts the focus back on its trigger, and
 * a pointer press outside both the panel and its trigger closes it too. While the panel stands
 * open Escape is the panel's, not the view's behind it, so the handler marks the press taken
 * (`defaultPrevented`, which the key registry reads) and stops it there. An Escape a layer over
 * the panel took first is that layer's, and the panel stays open.
 *
 * The listeners are only attached while `open`, so a closed panel costs nothing, and they act
 * only while the panel's host is shown. The view the person left lingers hidden with its panel
 * open (`HIDDEN_HOST`), and an Escape or a press then is the page's in front: the hidden panel
 * would take that Escape from the window's keys, close on it and move the focus into a view no
 * one sees. Coming back to the view finds the panel as it was left.
 */
export function usePanelDismiss({
  onOpenChange,
  open,
  panelRef,
  triggerRef,
}: PanelDismissOptions): void {
  useEffect(() => {
    if (!open) return;
    const closeOnEscape = (event: KeyboardEvent) => {
      // An Escape that ends an input method's composition is the input method's
      // (`inputMethodKey`): it cancels what the reader was composing, and the panel stays open.
      if (inputMethodKey(event)) return;
      if (event.key !== "Escape" || event.defaultPrevented) return;
      if (triggerRef.current?.closest(HIDDEN_HOST)) return;
      // The panel is what Escape acts on while it stands open, not the view behind it.
      event.preventDefault();
      event.stopPropagation();
      onOpenChange(false);
      triggerRef.current?.focus();
    };
    const closeOnOutsidePointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node) || triggerRef.current?.closest(HIDDEN_HOST)) return;
      if (triggerRef.current?.contains(target) || panelRef.current?.contains(target)) return;
      onOpenChange(false);
    };
    document.addEventListener("keydown", closeOnEscape);
    document.addEventListener("pointerdown", closeOnOutsidePointerDown);
    return () => {
      document.removeEventListener("keydown", closeOnEscape);
      document.removeEventListener("pointerdown", closeOnOutsidePointerDown);
    };
  }, [onOpenChange, open, panelRef, triggerRef]);
}
