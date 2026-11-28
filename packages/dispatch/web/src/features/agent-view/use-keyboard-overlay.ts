import type { RefObject } from "react";
import { PHONE_VIEWPORT_QUERY } from "../shell/useDialog";
import { type KeyboardWrites, useKeyboardFrame } from "../shell/useKeyboardFit";
import { atLiveEnd } from "./use-live-follow";

/** How much shorter than the screen the visual viewport must be before the view reads it as an
 *  on-screen keyboard: more than a browser's toolbars change the height as they collapse and come
 *  back, and less than a phone's keyboard takes. */
const KEYBOARD_MIN_HEIGHT = 150;

/** The view's own classes: the group its parts read the keyboard from, and the box it takes over
 *  the visual viewport while the keyboard is up (`data-keyboard="up"`, which the hook writes). */
export const KEYBOARD_VIEW_CLASSES =
  "group/live data-[keyboard=up]:fixed data-[keyboard=up]:inset-x-0 data-[keyboard=up]:z-10 data-[keyboard=up]:px-6 data-[keyboard=up]:py-2";

/** A part of the view that is hidden while the keyboard is up. */
export const HIDDEN_WITH_KEYBOARD = "group-data-[keyboard=up]/live:hidden";

/** The message box's height while the keyboard is up: four lines of its text. */
export const BOX_WITH_KEYBOARD = "group-data-[keyboard=up]/live:max-h-24";

/**
 * While the on-screen keyboard of a phone is up over the live view's composer, lays the view over
 * the visual viewport, the part of the page the person can see: `data-keyboard="up"` on `ref`'s
 * element, which `KEYBOARD_VIEW_CLASSES` and its parts' classes read, and the element's `top`
 * and `height` set to the visual viewport's. The view's header is then at the top of what the
 * person sees, the composer just above the keyboard, and the thread has all the height between
 * them. `<main>`'s keyboard cap (`useKeyboardFit`) moves only its bottom edge: the shell's header,
 * the page's gutters and the rows under the composer leave the thread no height of its own, and
 * iOS Safari's pan of the visual viewport down the page (`offsetTop`) puts the view's header
 * above the screen.
 *
 * The keyboard is up when the composer's textarea has focus and the visual viewport is at least
 * `KEYBOARD_MIN_HEIGHT` shorter than the tallest the layout viewport has been at this width. iOS
 * Safari shrinks only the visual viewport; Chromium on Android, told
 * `interactive-widget=resizes-content`, shrinks the layout viewport too, which is why the height
 * is compared with the tallest one seen at the width, kept for each width so that a phone turned
 * with the keyboard up still reads it. A hardware keyboard takes little or no height, so it
 * leaves the view in the page. Only below `md`: a tablet's and a desktop's layout stay as they
 * are.
 *
 * The thread (`data-live-thread`) keeps the reader's place through the switch: at its live end
 * if it was there when the box took focus, else at the same offset. A layout between the
 * keyboard's resize and the switch, with the view still in the page, gives the thread no height,
 * and WebKit then puts the thread back on its first turn, which the live view's follow would read
 * as the reader scrolling away.
 */
export function useKeyboardOverlay(ref: RefObject<HTMLElement | null>, enabled: boolean): void {
  useKeyboardFrame(ref, enabled, layOverVisualViewport);
}

function layOverVisualViewport(element: HTMLElement, viewport: VisualViewport): KeyboardWrites {
  const phone = window.matchMedia(PHONE_VIEWPORT_QUERY);
  const tallest = new Map<number, number>();
  let shown = false;
  // Where the thread was when the box took focus, before the keyboard came up.
  let kept: { atEnd: boolean; top: number } | null = null;
  const clear = () => {
    delete element.dataset.keyboard;
    element.style.removeProperty("top");
    element.style.removeProperty("height");
  };
  return {
    clear,
    write: (typing, event) => {
      const thread = element.querySelector<HTMLElement>("[data-live-thread]");
      if (event?.type === "focusin" && !shown && thread !== null) {
        kept = { atEnd: atLiveEnd(thread), top: thread.scrollTop };
      }
      const height = Math.max(tallest.get(window.innerWidth) ?? 0, window.innerHeight);
      tallest.set(window.innerWidth, height);
      const up = phone.matches && typing && viewport.height <= height - KEYBOARD_MIN_HEIGHT;
      if (up) {
        element.dataset.keyboard = "up";
        element.style.top = `${viewport.offsetTop}px`;
        element.style.height = `${viewport.height}px`;
        if (!shown && thread !== null && kept !== null) {
          thread.scrollTop = kept.atEnd ? thread.scrollHeight : kept.top;
        }
      } else {
        clear();
        // The keyboard is down: the next one keeps the place the box's next focus finds.
        if (shown) kept = null;
      }
      shown = up;
    },
  };
}
