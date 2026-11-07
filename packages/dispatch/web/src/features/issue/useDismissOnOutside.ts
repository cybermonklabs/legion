import { type RefObject, useEffect } from "react";

/**
 * Closes a menu the reader has left: a pointer pressed outside `wrapper`, or Escape anywhere
 * (dispatch://INFRA-1196 R11, R16).
 *
 * The issue page opens three menus - the identity block's actions, its add control and the
 * node menu beside the presets - and each of them would otherwise cover the row it sits on
 * until the reader pressed its control again. They are menus and not dialogs: nothing here
 * traps the focus or makes the page behind it inert, which is what `shell/useDialog.ts` is
 * for.
 *
 * `dismiss` is read through the effect's own closure, so it is registered afresh with every
 * render that changes it; `active` is what adds and removes the two listeners, so a closed
 * menu listens for nothing.
 */
export function useDismissOnOutside(
  wrapper: RefObject<HTMLElement | null>,
  active: boolean,
  dismiss: () => void
): void {
  useEffect(() => {
    if (!active) return;
    const closeOutside = (event: PointerEvent) => {
      if (!(event.target instanceof Node) || wrapper.current?.contains(event.target) !== true) {
        dismiss();
      }
    };
    const closeOnEscape = (event: KeyboardEvent) => {
      if (event.key === "Escape") dismiss();
    };
    document.addEventListener("pointerdown", closeOutside);
    document.addEventListener("keydown", closeOnEscape);
    return () => {
      document.removeEventListener("pointerdown", closeOutside);
      document.removeEventListener("keydown", closeOnEscape);
    };
  }, [active, dismiss, wrapper]);
}
