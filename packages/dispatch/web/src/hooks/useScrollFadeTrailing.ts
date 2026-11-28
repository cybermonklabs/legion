import { useLayoutEffect, useState } from "react";

import { scrollFadeTrailing } from "../theme/classes";

/**
 * The cue that a strip scrolling sideways holds more than it shows: `scrollFadeTrailing`, worn
 * only while the strip has somewhere left to scroll. Give the strip `ref` and put `fade` in its
 * class list. The scroll box of each Steps table (`features/steps/StepsTab.tsx`), a data table
 * the layout rule lets scroll sideways, wears it.
 *
 * The strip is state rather than a ref so the effect runs once per strip, not once per render:
 * three things change the answer and none of them is a render of the strip's owner - the reader
 * scrolling the strip, the strip's box or a child's changing size (the viewport, a live count, a
 * title arriving), and a child added or taken away. A scroll listener covers the first, a
 * ResizeObserver on the strip and each child the second, and a MutationObserver the third, after
 * which the children are observed again.
 */
export function useScrollFadeTrailing(): {
  fade: string;
  ref: (strip: HTMLElement | null) => void;
} {
  const [strip, setStrip] = useState<HTMLElement | null>(null);
  const [scrollsOn, setScrollsOn] = useState(false);
  useLayoutEffect(() => {
    if (strip === null) {
      return;
    }
    // A fractional layout leaves scrollLeft a hair short of the end; one pixel of slack keeps a
    // strip scrolled to its end from wearing the fade.
    const measure = () =>
      setScrollsOn(strip.scrollWidth - strip.clientWidth - strip.scrollLeft > 1);
    const sizes = new ResizeObserver(measure);
    const observeAll = () => {
      sizes.observe(strip);
      for (const child of strip.children) {
        sizes.observe(child);
      }
    };
    observeAll();
    const contents = new MutationObserver(() => {
      observeAll();
      measure();
    });
    contents.observe(strip, { characterData: true, childList: true, subtree: true });
    strip.addEventListener("scroll", measure, { passive: true });
    measure();
    return () => {
      strip.removeEventListener("scroll", measure);
      sizes.disconnect();
      contents.disconnect();
    };
  }, [strip]);
  return { fade: scrollsOn ? scrollFadeTrailing : "", ref: setStrip };
}
