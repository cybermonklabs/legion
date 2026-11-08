import { type RefObject, useEffect } from "react";

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

/**
 * How a small popover panel - the context ring's and the message box's question mark - is
 * dismissed: Escape closes it wherever the focus is and puts the focus back on its trigger, and
 * a pointer press outside both the panel and its trigger closes it too. While the panel stands
 * open Escape is the panel's, not the view's behind it, so the handler stops the event there.
 *
 * The listeners are only attached while `open`, so a closed panel costs nothing.
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
      if (event.key !== "Escape") return;
      // The panel is what Escape acts on while it stands open, not the view behind it.
      event.stopPropagation();
      onOpenChange(false);
      triggerRef.current?.focus();
    };
    const closeOnOutsidePointerDown = (event: PointerEvent) => {
      const target = event.target;
      if (!(target instanceof Node)) return;
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
