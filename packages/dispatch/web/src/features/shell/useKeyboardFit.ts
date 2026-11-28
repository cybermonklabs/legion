import { type RefObject, useLayoutEffect } from "react";

/** What one keyboard hook writes on its element as the on-screen keyboard and the focus change. */
export interface KeyboardWrites {
  /** Writes the element's state for now. `typing` says whether a textarea inside the element has
   *  focus; `event` is what called it, or undefined for the call as the hook starts. */
  readonly write: (typing: boolean, event: Event | undefined) => void;
  /** Takes back everything `write` wrote, as the hook stops. */
  readonly clear: () => void;
}

/**
 * The frame the keyboard hooks share. While `enabled`, it calls the writes `start` makes for the
 * element and the visual viewport on each resize and scroll of the visual viewport, each resize of
 * the window, each focus that enters a textarea inside the element, and once as it starts, for a
 * textarea that already had focus: no listener was there to hear that focus. It calls `clear` as
 * it stops. `start` runs once per start, so its writes can hold state of their own; pass a
 * function that does not change between renders.
 *
 * A blur, and a focus that goes to anything but a textarea, write nothing: the keyboard's own
 * resize, which follows them, does. A write there would move what a tap is still landing on:
 * Chrome focuses a tapped button or link, lays the page out again, and sends the click to what is
 * under the finger after that layout. With no on-screen keyboard (a desktop, a hardware
 * keyboard), no resize follows the blur, so the last write stays until the next resize, scroll or
 * focus on a textarea.
 *
 * Every write happens in the event's own task (no `requestAnimationFrame`, no state-keyed
 * effect), so the frame after a resize is already written.
 */
export function useKeyboardFrame(
  ref: RefObject<HTMLElement | null>,
  enabled: boolean,
  start: (element: HTMLElement, viewport: VisualViewport) => KeyboardWrites
): void {
  useLayoutEffect(() => {
    const element = ref.current;
    // `== null`: happy-dom leaves `visualViewport` undefined where the DOM types say `null`.
    const viewport = window.visualViewport;
    if (!enabled || element === null || viewport == null) return;
    const writes = start(element, viewport);
    const write = (event?: Event) => {
      const focused = document.activeElement;
      writes.write(focused instanceof HTMLTextAreaElement && element.contains(focused), event);
    };
    const focusIn = (event: FocusEvent) => {
      if (event.target instanceof HTMLTextAreaElement) write(event);
    };
    window.addEventListener("resize", write);
    viewport.addEventListener("resize", write);
    viewport.addEventListener("scroll", write);
    element.addEventListener("focusin", focusIn);
    write();
    return () => {
      window.removeEventListener("resize", write);
      viewport.removeEventListener("resize", write);
      viewport.removeEventListener("scroll", write);
      element.removeEventListener("focusin", focusIn);
      writes.clear();
    };
  }, [ref, enabled, start]);
}

/**
 * While `enabled`, caps the element's inline `max-height` as a textarea inside it takes focus,
 * so its bottom edge meets the visual viewport's bottom edge, and takes the cap off at the next
 * resize or scroll with no textarea in focus (`useKeyboardFrame`). On a desktop the cap that stays
 * after a blur is the element's own height, so it moves nothing. The shell passes
 * `<main>` on a route that fills the viewport, so the page's composer, the textarea at its
 * bottom, keeps `<main>`'s bottom gutter above the on-screen keyboard. The shell moves focus to
 * `<main>` on every page change, so a textarea that has focus as the hook starts is rare. A
 * disabled element is never capped.
 *
 * Chromium on Android does not need the cap: the page's `interactive-widget=resizes-content`
 * viewport shrinks the layout viewport, and the dynamic-viewport shell with it, so the visual
 * viewport's bottom is the element's own bottom. With the composer focused the hook still
 * writes a cap there, equal to the element's own height, so it changes nothing. iOS Safari
 * ignores that key and shrinks only the visual viewport, which it may also pan down the layout
 * viewport (`offsetTop`), so the visual viewport's bottom in layout coordinates is
 * `offsetTop + height`; element rectangles are measured in the same coordinates.
 */
export function useKeyboardFit(ref: RefObject<HTMLElement | null>, enabled: boolean): void {
  useKeyboardFrame(ref, enabled, capAtVisualViewport);
}

function capAtVisualViewport(element: HTMLElement, viewport: VisualViewport): KeyboardWrites {
  const clear = () => element.style.removeProperty("max-height");
  return {
    clear,
    write: (typing) => {
      if (!typing) {
        clear();
        return;
      }
      const bottom = viewport.offsetTop + viewport.height;
      element.style.maxHeight = `${Math.max(0, bottom - element.getBoundingClientRect().top)}px`;
    },
  };
}
