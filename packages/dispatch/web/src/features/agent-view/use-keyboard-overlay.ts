import { type RefObject, useLayoutEffect } from "react";

import { PHONE_VIEWPORT_QUERY } from "../shell/useDialog";

/** How much shorter than the screen the visual viewport must be before the view reads it as an
 *  on-screen keyboard: more than a browser's toolbars change the height as they collapse and come
 *  back, and less than a phone's keyboard takes. */
const KEYBOARD_MIN_HEIGHT = 150;

/**
 * While the on-screen keyboard of a phone is up over the live view's composer, lays the view over
 * the visual viewport, the part of the page the person can see: `data-keyboard="up"` on `ref`'s
 * element, which its classes and its children's read, and the element's `top` and `height` set to
 * the visual viewport's. The view's header is then at the top of what the person sees, the
 * composer at the bottom, just above the keyboard, and the thread has all the height between
 * them.
 *
 * `<main>`'s keyboard cap (`useKeyboardFit`) moves only its bottom edge. iOS Safari also pans the
 * visual viewport down the page to show a low input (`offsetTop`), and that put the view's header
 * and the top of the thread above the screen; with the shell's header, the page's gutters and the
 * row under the composer, the thread had no height left.
 *
 * The keyboard is up when the composer's textarea has focus and the visual viewport is at least
 * `KEYBOARD_MIN_HEIGHT` shorter than the tallest the layout viewport has been at this width. iOS
 * Safari shrinks only the visual viewport; Chromium on Android, told
 * `interactive-widget=resizes-content`, shrinks the layout viewport too, which is why the height
 * is compared with the tallest one seen rather than with the one now. A hardware keyboard takes
 * little or no height, so it leaves the view in the page. Only below `md`: a tablet's and a
 * desktop's layout stay as they are.
 *
 * Everything is written in the event's own task, as `useKeyboardFit` writes its cap, so no frame
 * shows the composer behind the keyboard.
 */
export function useKeyboardOverlay(ref: RefObject<HTMLElement | null>, enabled: boolean): void {
  useLayoutEffect(() => {
    const element = ref.current;
    // `== null`: happy-dom leaves `visualViewport` undefined where the DOM types say `null`.
    const viewport = window.visualViewport;
    if (!enabled || element === null || viewport == null) return;
    const phone = window.matchMedia(PHONE_VIEWPORT_QUERY);
    let width = window.innerWidth;
    let tallest = window.innerHeight;
    const place = () => {
      // A turn of the phone is another screen, with another tallest height.
      if (window.innerWidth !== width) {
        width = window.innerWidth;
        tallest = window.innerHeight;
      }
      tallest = Math.max(tallest, window.innerHeight);
      const focused = document.activeElement;
      const up =
        phone.matches &&
        focused instanceof HTMLTextAreaElement &&
        element.contains(focused) &&
        viewport.height <= tallest - KEYBOARD_MIN_HEIGHT;
      if (up) {
        element.dataset.keyboard = "up";
        element.style.top = `${viewport.offsetTop}px`;
        element.style.height = `${viewport.height}px`;
      } else {
        clear(element);
      }
    };
    // The window's own resize too: Chromium on Android resizes the layout viewport.
    window.addEventListener("resize", place);
    viewport.addEventListener("resize", place);
    viewport.addEventListener("scroll", place);
    element.addEventListener("focusin", place);
    element.addEventListener("focusout", place);
    place();
    return () => {
      window.removeEventListener("resize", place);
      viewport.removeEventListener("resize", place);
      viewport.removeEventListener("scroll", place);
      element.removeEventListener("focusin", place);
      element.removeEventListener("focusout", place);
      clear(element);
    };
  }, [ref, enabled]);
}

function clear(element: HTMLElement): void {
  delete element.dataset.keyboard;
  element.style.removeProperty("top");
  element.style.removeProperty("height");
}
